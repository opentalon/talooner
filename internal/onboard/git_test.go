package onboard

import (
	"context"
	"errors"
	"testing"
)

type fakeGitRunner struct {
	fail  map[string]error
	calls [][]string
}

func (f *fakeGitRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) > 0 {
		if err, ok := f.fail[args[0]]; ok {
			return "", err
		}
	}
	return "", nil
}

func TestCreateBranchRunsCheckout(t *testing.T) {
	r := &fakeGitRunner{}
	if err := CreateBranch(context.Background(), r, "talooner-onboarding", "main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0][0] != "checkout" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestCreateBranchPropagatesFailure(t *testing.T) {
	r := &fakeGitRunner{fail: map[string]error{"checkout": errors.New("branch exists")}}
	if err := CreateBranch(context.Background(), r, "talooner-onboarding", "main"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCommitAndPushStagesOnlyGivenPaths(t *testing.T) {
	r := &fakeGitRunner{}
	paths := []string{".github/talooner/rules.tln", ".github/talooner/rules.tln.test"}
	if err := CommitAndPush(context.Background(), r, "talooner-onboarding", "Add Talooner ruleset", paths); err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("calls = %v, want 3 (add, commit, push)", r.calls)
	}
	add := r.calls[0]
	if add[0] != "add" || add[len(add)-2] != paths[0] || add[len(add)-1] != paths[1] {
		t.Errorf("add call = %v", add)
	}
	if r.calls[1][0] != "commit" {
		t.Errorf("second call = %v, want commit", r.calls[1])
	}
	if r.calls[2][0] != "push" {
		t.Errorf("third call = %v, want push", r.calls[2])
	}
}

func TestCommitAndPushStopsOnAddFailure(t *testing.T) {
	r := &fakeGitRunner{fail: map[string]error{"add": errors.New("no such path")}}
	err := CommitAndPush(context.Background(), r, "b", "m", []string{"x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(r.calls) != 1 {
		t.Errorf("commit/push should not run after add fails, got calls %v", r.calls)
	}
}

func TestCommitAndPushStopsOnCommitFailure(t *testing.T) {
	r := &fakeGitRunner{fail: map[string]error{"commit": errors.New("nothing to commit")}}
	err := CommitAndPush(context.Background(), r, "b", "m", []string{"x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(r.calls) != 2 {
		t.Errorf("push should not run after commit fails, got calls %v", r.calls)
	}
}
