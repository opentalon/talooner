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
	"github.com/opentalon/talooner/internal/onboard"
)

func runRules(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "validate":
		return runRulesValidate(ctx, args[1:], stdout, stderr)
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
