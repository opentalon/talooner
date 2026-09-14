package onboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var ErrGitNotFound = errors.New("git not found in PATH")

type Git struct{}

func (Git) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", ErrGitNotFound
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

func CreateBranch(ctx context.Context, r Runner, branch, base string) error {
	if _, err := r.Run(ctx, "", "checkout", "-b", branch, base); err != nil {
		return fmt.Errorf("create branch %s from %s: %w", branch, base, err)
	}
	return nil
}

func CommitAndPush(ctx context.Context, r Runner, branch, message string, paths []string) error {
	args := append([]string{"add", "-f", "--"}, paths...)
	if _, err := r.Run(ctx, "", args...); err != nil {
		return fmt.Errorf("stage %v: %w", paths, err)
	}
	if _, err := r.Run(ctx, "", "commit", "-m", message); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if _, err := r.Run(ctx, "", "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}
