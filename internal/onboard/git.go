package onboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrGitNotFound means the git binary isn't on PATH.
var ErrGitNotFound = errors.New("git not found in PATH")

// Git is the real Runner for git, shelling out to the git binary. Kept
// separate from GH rather than a shared abstraction: different binary,
// different failure vocabulary ("git not found" vs "gh not authenticated"),
// and generalizing two small structs isn't worth it yet.
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

// CreateBranch checks out a new branch off base. onboard runs this in a
// clean, already-cloned working tree (same convention as init), so a plain
// `checkout -b` off the current HEAD is only correct once base is the
// checked-out branch — callers pass --base and are expected to have it
// checked out, or this fails with git's own "did not match any file(s)"
// rather than silently branching off the wrong commit.
func CreateBranch(ctx context.Context, r Runner, branch, base string) error {
	if _, err := r.Run(ctx, "", "checkout", "-b", branch, base); err != nil {
		return fmt.Errorf("create branch %s from %s: %w", branch, base, err)
	}
	return nil
}

// CommitAndPush stages exactly paths (never a broad `git add .`, so onboard
// can never sweep up unrelated working-tree changes), commits, and pushes
// the branch upstream. Staging uses -f: paths are always onboard's own known
// generated files, never caller input, and a Go repo's boilerplate .gitignore
// almost always has `*.test` for compiled test binaries — a pattern that also
// matches rules.tln.test's extension. Without -f, `git add` silently refuses
// and the whole commit fails on a repo that never touched talooner.
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
