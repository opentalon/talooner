package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/github"
)

// fakeSubmitter records the reviews a run asked for, so a test can assert that
// two verbs produced one write.
type fakeSubmitter struct {
	got []github.Review
	err error
}

func (f *fakeSubmitter) SyncReview(_ context.Context, _, _ string, _ int, rv github.Review) (int64, error) {
	f.got = append(f.got, rv)
	if f.err != nil {
		return 0, f.err
	}
	return 99, nil
}

func act(v action.Verb) action.Action { return action.Action{Verb: v, Target: "pr"} }

func TestVerdict(t *testing.T) {
	tests := map[string]struct {
		actions []action.Action
		want    string
	}{
		"approve":       {[]action.Action{act(action.VerbApprove)}, github.ReviewApprove},
		"block":         {[]action.Action{act(action.VerbBlock)}, github.ReviewRequestChanges},
		"neither":       {[]action.Action{act(action.VerbComment)}, ""},
		"nothing fired": {nil, ""},
		"tie, approve first": {
			[]action.Action{act(action.VerbApprove), act(action.VerbBlock)},
			github.ReviewRequestChanges,
		},
		"tie, block first": {
			[]action.Action{act(action.VerbBlock), act(action.VerbApprove)},
			github.ReviewRequestChanges,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Verdict(tc.actions); got != tc.want {
				t.Errorf("Verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both verbs firing is one review. Submitting the approval and dismissing it
// moments later would be two emails to every reviewer for one decision.
func TestExecuteWritesOnceForTheWholeSet(t *testing.T) {
	f := &fakeSubmitter{}
	w := New(f, "opentalon", "talooner", 42, "abc123", github.ReviewRequestChanges, nil)

	actions := []action.Action{act(action.VerbApprove), act(action.VerbBlock)}
	for _, a := range actions {
		if err := w.Execute(t.Context(), a); err != nil {
			t.Fatalf("Execute %s: %v", a.Verb, err)
		}
	}
	if err := w.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(f.got) != 1 {
		t.Fatalf("reviews written = %d, want 1: %+v", len(f.got), f.got)
	}
	if f.got[0].Event != github.ReviewRequestChanges {
		t.Errorf("event = %q, want block to win", f.got[0].Event)
	}
	if f.got[0].CommitID != "abc123" || f.got[0].Marker != Marker() {
		t.Errorf("review = %+v, want it pinned to the sha and marked", f.got[0])
	}
}

// The retraction case: nothing fired, so nothing performed the review — and the
// approval the last run left still has to come down.
func TestSyncRetractsWhenNoVerbFired(t *testing.T) {
	f := &fakeSubmitter{}
	w := New(f, "opentalon", "talooner", 42, "abc123", Verdict(nil), nil)

	if err := w.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(f.got) != 1 {
		t.Fatalf("reviews written = %d, want the retraction", len(f.got))
	}
	if f.got[0].Event != "" || f.got[0].Body != "" {
		t.Errorf("review = %+v, want an empty event and body", f.got[0])
	}
	if f.got[0].DismissMessage == "" {
		t.Error("a retraction with no message on it is one nobody can act on")
	}
}

func TestExecuteRefusesAVerbItDoesNotPerform(t *testing.T) {
	f := &fakeSubmitter{}
	w := New(f, "opentalon", "talooner", 42, "abc123", github.ReviewApprove, nil)

	err := w.Execute(t.Context(), act(action.VerbAssign))
	if !errors.Is(err, action.ErrUnknownVerb) {
		t.Fatalf("err = %v, want an unknown verb", err)
	}
	if len(f.got) != 0 {
		t.Errorf("wrote %+v for a verb it does not perform", f.got)
	}
}

func TestSyncReportsAFailedWrite(t *testing.T) {
	f := &fakeSubmitter{err: errors.New("resource not accessible by integration")}
	w := New(f, "opentalon", "talooner", 42, "abc123", github.ReviewApprove, nil)

	if err := w.Sync(t.Context()); err == nil {
		t.Fatal("Sync succeeded with a failing write")
	}
}

// The body carries no plugin-supplied text, so there is nothing in it for a
// fork PR's title to forge — and a review body cannot be edited to a resolved
// state the way a sticky comment can.
func TestBody(t *testing.T) {
	approve := Body(github.ReviewApprove, "abc123def456789")
	if !strings.Contains(approve, "advisory") {
		t.Error("the approval does not say it is advisory")
	}
	if !strings.Contains(approve, "abc123def456") || strings.Contains(approve, "789") {
		t.Errorf("approval body does not carry the short sha: %q", approve)
	}
	if strings.Contains(Body(github.ReviewRequestChanges, "abc123"), "advisory") {
		t.Error("a request for changes is not advisory in the same sense; say nothing rather than the wrong thing")
	}
	if got := Body("", "abc123"); got != "" {
		t.Errorf("Body of no verdict = %q, want empty: nothing is submitted", got)
	}
}
