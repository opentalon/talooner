// Command talooner is the operator CLI: cluster login, repo onboarding, and
// running rulesets against a live PR without writing anything.
//
// Output convention: stdout is the answer (tenant, quota, models…), stderr is
// everything else (usage, errors). A deliberate misuse (no command, unknown
// command, missing flag) exits 2; a command that ran but failed exits 1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/credentials"
	"github.com/opentalon/talooner/internal/onboard"
	"github.com/opentalon/talooner/internal/version"
)

const usage = `talooner is the operator CLI for a self-hosted Talooner deployment.

Usage:
  talooner cluster login --url <host> --key <api-key>
  talooner cluster whoami
  talooner init --repo <owner/name> [--org <org>]
  talooner onboard --repo <owner/name> [--base <branch>] [--branch <branch>] [--force] [--no-pr]
  talooner rules validate <path-to-.github/talooner>
  talooner rules test <path-to-.github/talooner>
  talooner rules plan --repo <owner/name> --pr <number>
  talooner version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "cluster":
		return runCluster(ctx, args[1:], stdout, stderr)
	case "init":
		return runInit(ctx, args[1:], stdout, stderr, onboard.GH{})
	case "onboard":
		return runOnboard(ctx, args[1:], stdout, stderr, onboard.GH{}, onboard.Git{})
	case "rules":
		return runRules(ctx, args[1:], stdout, stderr)
	case "version":
		printf(stdout, "talooner %s\n", version.Version)
		return 0
	case "-h", "--help", "help":
		printf(stdout, "%s", usage)
		return 0
	default:
		printf(stderr, "talooner: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runCluster(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printf(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "login":
		return runClusterLogin(args[1:], stdout, stderr)
	case "whoami":
		return runClusterWhoami(ctx, args[1:], stdout, stderr)
	default:
		printf(stderr, "talooner cluster: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// runClusterLogin stores host and key locally. It does not dial the cluster —
// that is whoami's job, so a login typed while the cluster happens to be
// unreachable still saves, and a tenant learns about a bad host or a revoked
// key from the same command that will always tell them (whoami), not from
// two different failure paths depending on when they last ran login.
func runClusterLogin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cluster login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "cluster gRPC endpoint, e.g. grpc://talon.example.com:9090")
	key := fs.String("key", "", "cluster API key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*url) == "" {
		printf(stderr, "talooner cluster login: --url is required\n")
		return 2
	}
	if strings.TrimSpace(*key) == "" {
		printf(stderr, "talooner cluster login: --key is required\n")
		return 2
	}

	path, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner cluster login: %v\n", err)
		return 1
	}
	if err := credentials.Save(path, credentials.Credentials{Host: *url, APIKey: *key}); err != nil {
		printf(stderr, "talooner cluster login: %v\n", err)
		return 1
	}
	printf(stdout, "credentials saved to %s\n", path)
	return 0
}

// runClusterWhoami is the onboarding experience: the first command a tenant
// runs after standing up a cluster, so every failure path gets a distinct
// message rather than one generic "failed" — a missing credentials file
// reads nothing like a revoked key, and neither reads like a nil dereference.
func runClusterWhoami(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cluster whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner cluster whoami: %v\n", err)
		return 1
	}
	creds, err := credentials.Load(path)
	if errors.Is(err, credentials.ErrNotFound) {
		printf(stderr, "talooner cluster whoami: no stored credentials, run `talooner cluster login` first\n")
		return 1
	}
	if err != nil {
		printf(stderr, "talooner cluster whoami: %v\n", err)
		return 1
	}

	// The CLI's own diagnostics, not the run's — nothing here needs to reach a
	// log aggregator, so discard rather than duplicate cluster.Dial's own
	// stderr-worthy lines under a second, uncoordinated writer.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := cluster.Dial(ctx, creds.Host, creds.APIKey, cluster.WithLogger(log))
	if err != nil {
		printf(stderr, "%s\n", describeDialFailure("cluster whoami", err))
		return 1
	}
	defer client.Close() //nolint:errcheck // best-effort on the way out of a one-shot command

	id := client.Identity()
	printf(stdout, "tenant:           %s\n", id.Tenant)
	printf(stdout, "protocol_version: %d\n", id.ProtocolVersion)
	printf(stdout, "models:           %s\n", strings.Join(id.Models, ", "))
	printf(stdout, "features:         %s\n", strings.Join(id.Features, ", "))
	printf(stdout, "quota:            %d / %d calls\n", id.Quota.LLMCallsUsed, id.Quota.LLMCallsLimit)
	return 0
}

// runInit sets the two secrets `talooner onboard`'s workflow needs
// (OPENTALON_HOST, OPENTALON_API_KEY) on the target repo — nothing else.
// It writes no local files and touches no git: it works purely against the
// GitHub API via gh, so it doesn't need to run inside a checkout of --repo
// at all. Workflow file, ruleset, and PR are all `talooner onboard`'s job,
// since a repo-shaped ruleset needs to investigate the repo first. gh is the
// gh CLI wrapper — a real onboard.GH{} from main, a fake from tests, so the
// test suite never shells out to a real gh binary.
func runInit(ctx context.Context, args []string, stdout, stderr io.Writer, gh onboard.Runner) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", "", "repo to onboard, as owner/name")
	org := fs.String("org", "", "set secrets at the org level instead of the repo level")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.Count(*repo, "/") != 1 || strings.HasPrefix(*repo, "/") || strings.HasSuffix(*repo, "/") {
		printf(stderr, "talooner init: --repo must be owner/name, got %q\n", *repo)
		return 2
	}

	path, err := credentials.DefaultPath()
	if err != nil {
		printf(stderr, "talooner init: %v\n", err)
		return 1
	}
	creds, err := credentials.Load(path)
	if errors.Is(err, credentials.ErrNotFound) {
		printf(stderr, "talooner init: no stored credentials, run `talooner cluster login` first\n")
		return 1
	}
	if err != nil {
		printf(stderr, "talooner init: %v\n", err)
		return 1
	}

	if err := onboard.CheckGH(ctx, gh); err != nil {
		printf(stderr, "talooner init: %v\n", err)
		return 1
	}

	secrets := []struct{ name, value string }{
		{"OPENTALON_HOST", creds.Host},
		{"OPENTALON_API_KEY", creds.APIKey},
	}
	for _, s := range secrets {
		var err error
		if *org != "" {
			err = onboard.SetOrgSecret(ctx, gh, *org, s.name, s.value)
		} else {
			err = onboard.SetRepoSecret(ctx, gh, *repo, s.name, s.value)
		}
		if err != nil {
			printf(stderr, "talooner init: %v\n", err)
			return 1
		}
		printf(stdout, "set secret %s\n", s.name)
	}

	printf(stdout, "secrets set on %s — run `talooner onboard --repo %s` to scaffold a ruleset and open a PR\n", *repo, *repo)
	return 0
}

// printf writes to w and discards a write failure: this is diagnostic and
// result output on a one-shot CLI command, and a caller whose stdout or
// stderr is gone (closed pipe) has nothing this process could do about it.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// describeDialFailure turns one of cluster.Dial's sentinel errors into the
// onboarding-relevant sentence, prefixed with which command hit it. Order
// matters: a rejected key wraps both ErrAction and ErrHandshake (a dial wraps
// the plugin's own refusal in the handshake error), so the specific case has
// to be checked before the catch-all "cannot reach cluster" one or every
// rejected key would read as unreachable.
func describeDialFailure(cmd string, err error) string {
	switch {
	case errors.Is(err, cluster.ErrMissingHost), errors.Is(err, cluster.ErrMissingKey):
		return fmt.Sprintf("talooner %s: stored credentials are incomplete, run `talooner cluster login` again: %v", cmd, err)
	case errors.Is(err, cluster.ErrProtocolSkew):
		return fmt.Sprintf("talooner %s: protocol mismatch: %v", cmd, err)
	case errors.Is(err, cluster.ErrAction):
		return fmt.Sprintf("talooner %s: rejected by cluster, the stored key may be revoked: %v", cmd, err)
	case errors.Is(err, cluster.ErrHandshake):
		return fmt.Sprintf("talooner %s: cannot reach cluster: %v", cmd, err)
	default:
		return fmt.Sprintf("talooner %s: %v", cmd, err)
	}
}
