package onboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrGHNotFound means the gh binary isn't on PATH — a different problem than
// gh being present but not logged in, and worth a different message.
var ErrGHNotFound = errors.New("gh CLI not found in PATH")

// Runner executes gh (or a fake, in tests). stdin is piped to the process
// rather than any argument carrying secret material, so a secret value never
// appears in argv, and so never in `ps` output or a shell history file.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (stdout string, err error)
}

// GH is the real Runner, shelling out to the gh binary.
type GH struct{}

func (GH) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", ErrGHNotFound
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// CheckGH fails fast with a distinct message for "gh isn't installed" versus
// "gh is installed but not logged in" — a maintainer hitting `talooner init`
// for the first time gets told which one to fix.
func CheckGH(ctx context.Context, r Runner) error {
	_, err := r.Run(ctx, "", "auth", "status")
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrGHNotFound) {
		return fmt.Errorf("gh CLI not found — install https://cli.github.com: %w", err)
	}
	return fmt.Errorf("gh is not authenticated, run `gh auth login`: %w", err)
}

// SetRepoSecret sets a secret on a single repo. repo is "owner/name".
func SetRepoSecret(ctx context.Context, r Runner, repo, name, value string) error {
	if _, err := r.Run(ctx, value, "secret", "set", name, "--repo", repo); err != nil {
		return fmt.Errorf("set secret %s on %s: %w", name, repo, err)
	}
	return nil
}

// SetOrgSecret sets a secret visible to every repo in org. gh requires an
// explicit --visibility for org secrets; "all" is the setup auth.md
// describes org-wide secrets for — onboarding more than a couple of repos
// under one cluster, all of which are meant to read it.
func SetOrgSecret(ctx context.Context, r Runner, org, name, value string) error {
	if _, err := r.Run(ctx, value, "secret", "set", name, "--org", org, "--visibility", "all"); err != nil {
		return fmt.Errorf("set secret %s on org %s: %w", name, org, err)
	}
	return nil
}
