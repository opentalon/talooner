package onboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var ErrGHNotFound = errors.New("gh CLI not found in PATH")

type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (stdout string, err error)
}

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

func SetRepoSecret(ctx context.Context, r Runner, repo, name, value string) error {
	if _, err := r.Run(ctx, value, "secret", "set", name, "--repo", repo); err != nil {
		return fmt.Errorf("set secret %s on %s: %w", name, repo, err)
	}
	return nil
}

func SetOrgSecret(ctx context.Context, r Runner, org, name, value string) error {
	if _, err := r.Run(ctx, value, "secret", "set", name, "--org", org, "--visibility", "all"); err != nil {
		return fmt.Errorf("set secret %s on org %s: %w", name, org, err)
	}
	return nil
}
