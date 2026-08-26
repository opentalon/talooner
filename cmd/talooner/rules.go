package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/credentials"
	"github.com/opentalon/talooner/internal/github"
	"github.com/opentalon/talooner/internal/onboard"
	talrun "github.com/opentalon/talooner/internal/run"
)

func runRules(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "validate":
		return runRulesValidate(ctx, args[1:], stdout, stderr)
	case "test":
		return runRulesTest(ctx, args[1:], stdout, stderr)
	case "plan":
		return runRulesPlan(ctx, args[1:], stdout, stderr)
	default:
		printf(stderr, "talooner rules: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// runRulesValidate compiles a tenant's ruleset against the cluster without
// evaluating anything, so a bad ruleset is caught before it ever gates a PR.
// It round-trips to the cluster's validate_ruleset action rather than
// embedding a second compiler — that is a deliberate decision (issue #24),
// resolving the conflict between architecture.md (bot links neither
// tln-language nor tln-db) and auth.md's older "all local" claim: a tenant's
// CI and the plugin can never disagree about whether a ruleset is valid,
// because it is literally the same code path.
func runRulesValidate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rules validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		printf(stderr, "talooner rules validate: usage: talooner rules validate <path-to-.github/talooner>\n")
		return 2
	}

	path := filepath.Join(fs.Arg(0), filepath.Base(onboard.RulesetPath))
	src, err := os.ReadFile(path)
	if err != nil {
		printf(stderr, "talooner rules validate: reading %s: %v\n", path, err)
		return 1
	}

	credPath, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner rules validate: %v\n", err)
		return 1
	}
	creds, err := credentials.Load(credPath)
	if errors.Is(err, credentials.ErrNotFound) {
		printf(stderr, "talooner rules validate: no stored credentials, run `talooner cluster login` first\n")
		return 1
	}
	if err != nil {
		printf(stderr, "talooner rules validate: %v\n", err)
		return 1
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := cluster.Dial(ctx, creds.Host, creds.APIKey, cluster.WithLogger(log))
	if err != nil {
		printf(stderr, "%s\n", describeDialFailure("rules validate", err))
		return 1
	}
	defer client.Close() //nolint:errcheck // best-effort on the way out of a one-shot command

	resp, err := client.ValidateRuleset(ctx, string(src))
	if err != nil {
		printf(stderr, "talooner rules validate: %v\n", err)
		return 1
	}

	for _, d := range resp.GetDiagnostics() {
		printf(stderr, "%s: %s\n", diagnosticPosition(path, d), strings.TrimSpace(d.GetMessage()))
	}
	if !resp.GetValid() {
		printf(stderr, "talooner rules validate: %s is not valid\n", path)
		return 1
	}
	printf(stdout, "%s is valid\n", path)
	return 0
}

// runRulesTest runs a tenant's rules.tln.test against rules.tln, round-tripped
// to the cluster's run_ruleset_test action the same way runRulesValidate
// round-trips to validate_ruleset (issue #24's second half) — a tenant's CI
// and the plugin can never disagree about whether a rule passes its own
// tests, because it is the same code path.
func runRulesTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rules test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		printf(stderr, "talooner rules test: usage: talooner rules test <path-to-.github/talooner>\n")
		return 2
	}

	rulesetPath := filepath.Join(fs.Arg(0), filepath.Base(onboard.RulesetPath))
	src, err := os.ReadFile(rulesetPath)
	if err != nil {
		printf(stderr, "talooner rules test: reading %s: %v\n", rulesetPath, err)
		return 1
	}
	testPath := filepath.Join(fs.Arg(0), filepath.Base(onboard.RulesetTestPath))
	testSrc, err := os.ReadFile(testPath)
	if err != nil {
		printf(stderr, "talooner rules test: reading %s: %v\n", testPath, err)
		return 1
	}

	credPath, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner rules test: %v\n", err)
		return 1
	}
	creds, err := credentials.Load(credPath)
	if errors.Is(err, credentials.ErrNotFound) {
		printf(stderr, "talooner rules test: no stored credentials, run `talooner cluster login` first\n")
		return 1
	}
	if err != nil {
		printf(stderr, "talooner rules test: %v\n", err)
		return 1
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := cluster.Dial(ctx, creds.Host, creds.APIKey, cluster.WithLogger(log))
	if err != nil {
		printf(stderr, "%s\n", describeDialFailure("rules test", err))
		return 1
	}
	defer client.Close() //nolint:errcheck // best-effort on the way out of a one-shot command

	resp, err := client.RunRulesetTest(ctx, string(src), string(testSrc))
	if err != nil {
		printf(stderr, "talooner rules test: %v\n", err)
		return 1
	}

	// The plugin's Diagnostic has no file field on the wire (talooner-plugin's
	// toProtoDiagnostics drops it), even though the underlying compiler knows
	// whether a diagnostic came from rules.tln or rules.tln.test — so a
	// position here can only be line:column, not a full path. Flagged
	// upstream rather than guessed at.
	if len(resp.GetDiagnostics()) > 0 {
		for _, d := range resp.GetDiagnostics() {
			printf(stderr, "%s: %s\n", testDiagnosticPosition(d), strings.TrimSpace(d.GetMessage()))
		}
		printf(stderr, "talooner rules test: %s did not compile\n", rulesetPath)
		return 1
	}

	failed := 0
	for _, r := range resp.GetResults() {
		if r.GetPassed() {
			printf(stdout, "PASS %s\n", r.GetName())
			continue
		}
		failed++
		printf(stdout, "FAIL %s\n", r.GetName())
		for _, e := range r.GetErrors() {
			printf(stdout, "     %s\n", e)
		}
	}
	if failed > 0 {
		printf(stderr, "talooner rules test: %d/%d tests failed\n", failed, len(resp.GetResults()))
		return 1
	}
	printf(stdout, "%d/%d tests passed\n", len(resp.GetResults()), len(resp.GetResults()))
	return 0
}

// runRulesPlan runs a live PR's base-branch ruleset in plan mode and prints
// the actions that would fire (F4, #25). It reuses run.Runner.Plan — the
// printer executor (D1) swapped into the same registry the real run executes
// with — so this is never a second code path that can drift from what
// execution actually does, and mode: plan makes "zero writes" a property of
// the wire protocol rather than a convention this command has to uphold on
// its own.
func runRulesPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rules plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repo to evaluate, as owner/name")
	pr := fs.Int("pr", 0, "pull request number")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	owner, name, ok := strings.Cut(*repo, "/")
	if !ok || owner == "" || name == "" {
		printf(stderr, "talooner rules plan: --repo must be owner/name, got %q\n", *repo)
		return 2
	}
	if *pr <= 0 {
		printf(stderr, "talooner rules plan: --pr is required\n")
		return 2
	}

	credPath, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner rules plan: %v\n", err)
		return 1
	}
	creds, err := credentials.Load(credPath)
	if errors.Is(err, credentials.ErrNotFound) {
		printf(stderr, "talooner rules plan: no stored credentials, run `talooner cluster login` first\n")
		return 1
	}
	if err != nil {
		printf(stderr, "talooner rules plan: %v\n", err)
		return 1
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := cluster.Dial(ctx, creds.Host, creds.APIKey, cluster.WithLogger(log))
	if err != nil {
		printf(stderr, "%s\n", describeDialFailure("rules plan", err))
		return 1
	}
	defer client.Close() //nolint:errcheck // best-effort on the way out of a one-shot command

	gh, err := github.NewFromEnv(github.WithLogger(log), github.WithSecrets(client.APIKey()))
	if err != nil {
		printf(stderr, "talooner rules plan: %v\n", err)
		return 1
	}

	r := talrun.Runner{GitHub: gh, Cluster: client, Log: log}
	if err := r.Plan(ctx, owner, name, *pr, stdout); err != nil {
		printf(stderr, "talooner rules plan: %v\n", err)
		return 1
	}
	return 0
}

// testDiagnosticPosition formats a run_ruleset_test diagnostic's location as
// line[:column] only — no file, since the wire response can't say whether the
// diagnostic came from rules.tln or rules.tln.test (see runRulesTest).
func testDiagnosticPosition(d *taloonerpb.Diagnostic) string {
	if d.GetLine() <= 0 {
		return "rules test"
	}
	if d.GetColumn() <= 0 {
		return fmt.Sprintf("line %d", d.GetLine())
	}
	return fmt.Sprintf("line %d:%d", d.GetLine(), d.GetColumn())
}

// diagnosticPosition formats a diagnostic's location the way a compiler
// error normally reads (path:line:column). Line 0 means the compiler could
// not place it (internal/check.Diagnostic carries the same convention).
func diagnosticPosition(path string, d *taloonerpb.Diagnostic) string {
	if d.GetLine() <= 0 {
		return path
	}
	if d.GetColumn() <= 0 {
		return fmt.Sprintf("%s:%d", path, d.GetLine())
	}
	return fmt.Sprintf("%s:%d:%d", path, d.GetLine(), d.GetColumn())
}
