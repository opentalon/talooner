package action

import (
	"slices"
	"testing"
)

func TestDiffIdenticalSetsIsEmpty(t *testing.T) {
	base := []Action{{Verb: VerbApprove, Target: "pr"}, {Verb: VerbComment, Target: "pr", Text: "x"}}
	added, removed := Diff(base, slices.Clone(base))
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("added = %+v, removed = %+v, want both empty", added, removed)
	}
}

func TestDiffReportsWhatThePlanAddsAndDrops(t *testing.T) {
	base := []Action{{Verb: VerbApprove, Target: "pr"}}
	planned := []Action{{Verb: VerbBlock, Target: "pr", Text: "needs tests"}}

	added, removed := Diff(base, planned)
	if len(added) != 1 || added[0] != planned[0] {
		t.Errorf("added = %+v, want %+v", added, planned)
	}
	if len(removed) != 1 || removed[0] != base[0] {
		t.Errorf("removed = %+v, want %+v", removed, base)
	}
}

// Two actions that differ only in an argument are not the same decision: a
// block with a different reason is a different finding, not a match.
func TestDiffTreatsDifferentArgumentsAsDifferentActions(t *testing.T) {
	base := []Action{{Verb: VerbBlock, Target: "pr", Text: "needs tests"}}
	planned := []Action{{Verb: VerbBlock, Target: "pr", Text: "needs docs"}}

	added, removed := Diff(base, planned)
	if len(added) != 1 || len(removed) != 1 {
		t.Fatalf("added = %+v, removed = %+v, want one of each", added, removed)
	}
}

// A duplicate in one set and a single occurrence in the other is one added or
// removed action, not zero — the count itself is part of the decision.
func TestDiffCountsMultiplicity(t *testing.T) {
	a := Action{Verb: VerbComment, Target: "pr", Text: "x"}
	base := []Action{a}
	planned := []Action{a, a}

	added, removed := Diff(base, planned)
	if len(added) != 1 || added[0] != a {
		t.Errorf("added = %+v, want one extra %+v", added, a)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %+v, want none", removed)
	}
}

func TestDiffEmptyBaseEverythingIsAdded(t *testing.T) {
	planned := []Action{{Verb: VerbAssign, Assignee: "alice"}, {Verb: VerbRequire, Target: "review.security"}}
	added, removed := Diff(nil, planned)
	if !slices.Equal(added, planned) {
		t.Errorf("added = %+v, want %+v", added, planned)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %+v, want none", removed)
	}
}

func TestDiffEmptyPlannedEverythingIsRemoved(t *testing.T) {
	base := []Action{{Verb: VerbAssign, Assignee: "alice"}, {Verb: VerbRequire, Target: "review.security"}}
	added, removed := Diff(base, nil)
	if len(added) != 0 {
		t.Errorf("added = %+v, want none", added)
	}
	if !slices.Equal(removed, base) {
		t.Errorf("removed = %+v, want %+v", removed, base)
	}
}
