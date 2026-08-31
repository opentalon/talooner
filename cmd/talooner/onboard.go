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

// defaultOnboardBase and defaultOnboardBranch are onboard's flag defaults.
// base has to match whatever branch is actually checked out locally — `init`
// leaves that to the maintainer, and onboard does the same rather than
// guessing.
const (
	defaultOnboardBase   = "main"
	defaultOnboardBranch = "talooner-onboarding"
)

// runOnboard investigates the repo it's run in, asks the cluster to
// scaffold a rules.tln + rules.tln.test pair via generate_ruleset (falling
// back to onboard's static starter when the plugin reports source ==
// "fallback"), verifies the pair through the same validate/test round-trip
// `talooner rules validate`/`talooner rules test` use, and — unless --no-pr
// — commits, pushes, and opens a PR titled "talooner onboarding". It writes
// local files before touching git, same as `init`, so a maintainer can
// always `git diff` before anything is pushed.
func runOnboard(ctx context.Context, args []string, stdout, stderr io.Writer, gh, git onboard.Runner) int {
	fs := flag.NewFlagSet("onboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repo to onboard, as owner/name")
	base := fs.String("base", defaultOnboardBase, "base branch the PR targets, and the branch onboard's new branch is cut from")
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
	defer client.Close() //nolint:errcheck // best-effort on the way out of a one-shot command

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

	if err := onboard.CreateBranch(ctx, git, *branch, *base); err != nil {
		printf(stderr, "talooner onboard: %v\n", err)
		return 1
	}
	commitMsg := "Add Talooner ruleset (talooner onboarding)"
	if err := onboard.CommitAndPush(ctx, git, *branch, commitMsg, []string{onboard.RulesetPath, onboard.RulesetTestPath}); err != nil {
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
		"--base", *base, "--head", *branch, "--body", body)
	if err != nil {
		printf(stderr, "talooner onboard: opening PR: %v\n", err)
		return 1
	}
	printf(stdout, "%s", out)
	return 0
}

// onboardPRBody explains where the ruleset came from and what grounded it,
// so a reviewer isn't left guessing whether they're looking at a model's
// guess or the plain starter.
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
