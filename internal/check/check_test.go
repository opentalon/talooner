package check

import (
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/github"
)

func act(v action.Verb) action.Action {
	a := action.Action{Verb: v, Target: "pr"}
	switch v {
	case action.VerbComment:
		a.Text = "describe your change"
	case action.VerbAssign:
		a.Assignee = "alice"
	case action.VerbEmit:
		a.Name = "needs_docs"
	}
	return a
}

func TestDecisionConclusions(t *testing.T) {
	tests := []struct {
		name    string
		actions []action.Action
		want    string
	}{
		{"a block fired", []action.Action{act(action.VerbBlock)}, github.ConclusionFailure},
		{"an approve fired", []action.Action{act(action.VerbApprove)}, github.ConclusionSuccess},
		{"nothing decisive", []action.Action{act(action.VerbComment)}, github.ConclusionNeutral},
		{"no rule fired", nil, github.ConclusionSuccess},
		{
			"an unresolved tie is a failure, not a success",
			[]action.Action{act(action.VerbApprove), act(action.VerbBlock)},
			github.ConclusionFailure,
		},
		{
			"order does not change the tiebreak",
			[]action.Action{act(action.VerbBlock), act(action.VerbApprove)},
			github.ConclusionFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := Decision(tt.actions, nil, "")
			if cr.Conclusion != tt.want {
				t.Errorf("conclusion = %q, want %q", cr.Conclusion, tt.want)
			}
			if cr.Name != Name {
				t.Errorf("name = %q, want %q", cr.Name, Name)
			}
			if strings.TrimSpace(cr.Title) == "" || strings.TrimSpace(cr.Summary) == "" {
				t.Errorf("check run has no title or summary: %+v", cr)
			}
		})
	}
}

func TestDecisionSaysWhenNothingFired(t *testing.T) {
	cr := Decision(nil, nil, "")
	if !strings.Contains(cr.Summary, "No rule matched") {
		t.Errorf("summary should say no rule matched, got %q", cr.Summary)
	}
	if len(cr.Annotations) != 0 {
		t.Errorf("a clean decision should carry no annotations, got %d", len(cr.Annotations))
	}
}

// The warning is the product of an unresolved tie; the block-wins conclusion is
// only there so the check has one value. A check that failed without saying why
// leaves the maintainer with nothing to fix.
func TestDecisionSurfacesTheTieWarning(t *testing.T) {
	cr := Decision(
		[]action.Action{act(action.VerbApprove), act(action.VerbBlock)},
		[]Warning{{Code: "unresolved_conflict", Message: "approve and block both fired"}},
		"2 rules fired",
	)
	for _, want := range []string{"2 rules fired", "unresolved_conflict", "approve and block both fired", "overrides"} {
		if !strings.Contains(cr.Summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, cr.Summary)
		}
	}
}

func TestDecisionWarningWithNoCode(t *testing.T) {
	cr := Decision([]action.Action{act(action.VerbComment)}, []Warning{{Message: "the llm_review feature is off"}}, "")
	if !strings.Contains(cr.Summary, "the llm_review feature is off") {
		t.Errorf("summary is missing the warning:\n%s", cr.Summary)
	}
}

// Fail open on Talooner's own faults: there is no input to Broken that produces
// a failing check run.
func TestBrokenIsAlwaysNeutral(t *testing.T) {
	for _, diags := range [][]Diagnostic{
		nil,
		{{Path: "rules.tln", Line: 4, Column: 9, Message: "unexpected token"}},
	} {
		cr := Broken("evaluate opentalon/talooner#42: ruleset would not compile", diags)
		if cr.Conclusion != github.ConclusionNeutral {
			t.Fatalf("conclusion = %q, want neutral", cr.Conclusion)
		}
		if !strings.Contains(cr.Summary, "would not compile") {
			t.Errorf("summary should carry the reason:\n%s", cr.Summary)
		}
	}
}

func TestBrokenPinsDiagnosticsToTheirLines(t *testing.T) {
	cr := Broken("ruleset would not compile", []Diagnostic{
		{Path: "rules.tln", Line: 4, Column: 9, Message: "unexpected token \"do\""},
		{Path: "rules.tln", Line: 0, Message: "no rule named review"}, // unplaceable
	})

	if len(cr.Annotations) != 2 {
		t.Fatalf("annotations = %d, want 2", len(cr.Annotations))
	}
	first := cr.Annotations[0]
	if first.StartLine != 4 || first.EndLine != 4 {
		t.Errorf("first annotation lines = %d-%d, want 4-4", first.StartLine, first.EndLine)
	}
	if !strings.Contains(first.Message, "column 9") || !strings.Contains(first.Message, "unexpected token") {
		t.Errorf("first annotation message = %q", first.Message)
	}
	// An annotation at line 0 is rejected by the API, so an unplaceable
	// diagnostic still points at the file rather than being dropped.
	if cr.Annotations[1].StartLine != 1 {
		t.Errorf("unplaceable diagnostic landed on line %d, want 1", cr.Annotations[1].StartLine)
	}
	for _, a := range cr.Annotations {
		if a.Level != github.LevelFailure {
			t.Errorf("annotation level = %q, want failure", a.Level)
		}
	}
}

func TestBrokenSaysHowManyDiagnosticsItLeftOut(t *testing.T) {
	diags := make([]Diagnostic, maxAnnotations+3)
	for i := range diags {
		diags[i] = Diagnostic{Path: "rules.tln", Line: i + 1, Message: "boom"}
	}

	cr := Broken("ruleset would not compile", diags)
	if len(cr.Annotations) != maxAnnotations {
		t.Fatalf("annotations = %d, want %d", len(cr.Annotations), maxAnnotations)
	}
	if !strings.Contains(cr.Summary, "3 further diagnostic") {
		t.Errorf("summary should name the 3 left out:\n%s", cr.Summary)
	}
}
