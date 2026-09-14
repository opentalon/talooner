package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/credentials"
	"github.com/opentalon/talooner/internal/onboard"
)

const defaultOnboardBranch = "talooner-onboarding"

func runOnboard(ctx context.Context, args []string, stdout, stderr io.Writer, gh, git onboard.Runner) int {
	fs := flag.NewFlagSet("onboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repo to onboard, as owner/name")
	base := fs.String("base", "", "base branch the PR targets, and the branch onboard's new branch is cut from; auto-detected (local master, then main) when omitted")
	branch := fs.String("branch", defaultOnboardBranch, "branch to create for the ruleset")
	force := fs.Bool("force", false, "overwrite an existing rules.tln/rules.tln.test that differs")
	noPR := fs.Bool("no-pr", false, "write and verify the ruleset locally but skip branch/commit/push/PR")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.Count(*repo, "/") != 1 || strings.HasPrefix(*repo, "/") || strings.HasSuffix(*repo, "/") {
		printf(stderr, "talooner onboard: --repo must be owner/name, got %q\n", *repo)
		return 2
	}

	credPath, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}
	creds, err := credentials.Load(credPath)
	if errors.Is(err, credentials.ErrNotFound) {
		printf(stderr, "talooner onboard: no stored credentials, run `talooner cluster login` first\n")
		return 1
	}
	if err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}

	summary, err := onboard.Investigate(".")
	if err != nil {
		printf(stderr, "talooner onboard: investigating repo: %v\n", err)
		return 1
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := cluster.Dial(ctx, creds.Host, creds.APIKey, cluster.WithLogger(log))
	if err != nil {
		printf(stderr, "%s\n", describeDialFailure("onboard", err))
		return 1
	}
	defer client.Close() //nolint:errcheck

	genResp, err := client.GenerateRuleset(ctx, summary)
	if err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}

	rulesetSrc, testSrc := genResp.GetRuleset(), genResp.GetRulesetTest()
	if genResp.GetSource() == "fallback" {
		printf(stdout, "generate_ruleset fell back to the starter ruleset: %s\n", genResp.GetNote())
		rulesetSrc, testSrc = string(onboard.Ruleset), string(onboard.RulesetTest)
	} else {
		printf(stdout, "generated a ruleset from the repo summary\n")
	}

	files := []struct {
		path    string
		content []byte
	}{
		{onboard.WorkflowPath, onboard.Workflow},
		{onboard.RulesetPath, []byte(rulesetSrc)},
		{onboard.RulesetTestPath, []byte(testSrc)},
	}
	for _, f := range files {
		outcome, diff, err := onboard.WriteFile(f.path, f.content, *force)
		if err != nil {
			printf(stderr, "talooner onboard: writing %s: %v\n", f.path, err)
			return 1
		}
		switch outcome {
		case onboard.Created:
			printf(stdout, "wrote %s\n", f.path)
		case onboard.Unchanged:
			printf(stdout, "%s already up to date\n", f.path)
		case onboard.Conflict:
			printf(stderr, "talooner onboard: %s already exists and differs from the generated ruleset:\n\n%s\n"+
				"rerun with --force to overwrite it\n", f.path, diff)
			return 1
		}
	}

	if !validateAndPrint(ctx, client, "talooner onboard", onboard.RulesetPath, rulesetSrc, stdout, stderr) {
		return 1
	}
	if !testAndPrint(ctx, client, "talooner onboard", onboard.RulesetPath, rulesetSrc, testSrc, stdout, stderr) {
		return 1
	}

	if *noPR {
		printf(stdout, "--no-pr: wrote and verified the ruleset locally, skipping branch/commit/push/PR\n")
		return 0
	}

	resolvedBase, err := resolveBaseBranch(ctx, git, *base)
	if err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}

	if localBranchExists(ctx, git, *branch) {
		printf(stderr, "talooner onboard: branch %q already exists locally — a previous onboarding run may still have an open PR; check `gh pr list --repo %s --head %s`, then close/merge it or delete the local branch (`git branch -D %s`) before retrying\n",
			*branch, *repo, *branch, *branch)
		return 1
	}

	if err := onboard.CreateBranch(ctx, git, *branch, resolvedBase); err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}
	commitMsg := "Add Talooner workflow and ruleset (talooner onboarding)"
	if err := onboard.CommitAndPush(ctx, git, *branch, commitMsg, []string{onboard.WorkflowPath, onboard.RulesetPath, onboard.RulesetTestPath}); err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}

	if err := onboard.CheckGH(ctx, gh); err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}
	body := onboardPRBody(genResp, summary)
	out, err := gh.Run(ctx, "", "pr", "create",
		"--repo", *repo, "--title", "talooner onboarding",
		"--base", resolvedBase, "--head", *branch, "--body", body)
	if err != nil {
		printf(stderr, "talooner onboard: opening PR: %v\n", err)
		return 1
	}
	printf(stdout, "%s", out)
	return 0
}

func resolveBaseBranch(ctx context.Context, git onboard.Runner, explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	for _, candidate := range []string{"master", "main"} {
		if localBranchExists(ctx, git, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not detect a base branch (no local master or main); specify --base")
}

func localBranchExists(ctx context.Context, git onboard.Runner, branch string) bool {
	_, err := git.Run(ctx, "", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func onboardPRBody(genResp *taloonerpb.GenerateRulesetResponse, summary string) string {
	var b strings.Builder
	if genResp.GetSource() == "llm" {
		fmt.Fprintln(&b, "Ruleset generated from this repo's layout by Talooner's generate_ruleset action.")
	} else {
		fmt.Fprintf(&b, "generate_ruleset fell back to the static starter ruleset: %s\n", genResp.GetNote())
	}
	fmt.Fprintln(&b, "\nBoth rules.tln and rules.tln.test compiled and passed their own tests before this PR was opened.")
	fmt.Fprintln(&b, "\n<details><summary>Repo summary used</summary>")
	fmt.Fprintln(&b, "```")
	fmt.Fprint(&b, summary)
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b, "</details>")
	return b.String()
}
