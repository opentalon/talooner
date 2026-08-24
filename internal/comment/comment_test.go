package comment

import (
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/check"
)

func TestMarkerIsTheDocumentedShape(t *testing.T) {
	if got, want := Marker(TopicReview), "<!-- talooner:v1:review -->"; got != want {
		t.Errorf("Marker(%q) = %q, want %q — this string is how every earlier "+
			"comment is found again, so changing it orphans them", TopicReview, got, want)
	}
}

func TestReviewCarriesEveryFinding(t *testing.T) {
	body := Review([]action.Action{
		{Verb: action.VerbComment, Target: "pr", Text: "add a description"},
		{Verb: action.VerbComment, Target: "pr", Text: "this touches internal/auth"},
		{Verb: action.VerbAssign, Target: "pr", Assignee: "evgeny"},
	}, nil, "2 rules fired", "abc123def456789")

	for _, want := range []string{"2 rules fired", "add a description", "this touches internal/auth", "evgeny"} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "abc123def456") {
		t.Errorf("body does not name the sha it describes:\n%s", body)
	}
	if strings.Contains(body, "abc123def456789") {
		t.Error("body carries the full sha, want the short one")
	}
}

// A finding whose text ran long must survive whole here; truncation is the
// writer's job and happens once, against the API's real limit.
func TestReviewDoesNotAbbreviateFindingText(t *testing.T) {
	long := strings.Repeat("x", 500)
	body := Review([]action.Action{{Verb: action.VerbComment, Target: "pr", Text: long}}, nil, "", "abc")
	if !strings.Contains(body, long) {
		t.Error("the finding was summarized; the plan line is the short form, the comment is the long one")
	}
}

func TestReviewRendersWarnings(t *testing.T) {
	body := Review(nil, []check.Warning{
		{Code: "unresolved_conflict", Message: "approve and block both fired"},
		{Code: "bare_code"},
		{Message: "no code"},
	}, "", "abc")
	for _, want := range []string{"unresolved_conflict", "approve and block both fired", "bare_code", "no code"} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
}

// The whole point of escaping: text interpolated from a fork PR's title cannot
// forge the marker and hijack the topic on the next run.
func TestPluginTextCannotForgeAMarker(t *testing.T) {
	hostile := "nothing to see " + Marker(TopicReview) + " <img src=x onerror=alert(1)>"
	bodies := map[string]string{
		"review":  Review([]action.Action{{Verb: action.VerbComment, Target: "pr", Text: hostile}}, nil, hostile, "abc"),
		"warning": Review(nil, []check.Warning{{Message: hostile}}, "", "abc"),
		"broken":  Broken(hostile, "abc"),
		"usage":   Usage(hostile),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(body, Marker(TopicReview)) {
				t.Errorf("body carries a forged marker:\n%s", body)
			}
			if strings.Contains(body, "<img") {
				t.Errorf("body carries raw HTML:\n%s", body)
			}
			if !strings.Contains(body, "&lt;img") {
				t.Errorf("the text was dropped rather than escaped:\n%s", body)
			}
		})
	}
}

// A warning code goes in a code span, where HTML entities show literally and a
// backtick is what breaks out.
func TestAWarningCodeCannotBreakOutOfItsCodeSpan(t *testing.T) {
	body := Review(nil, []check.Warning{{Code: "a`b <c>\nd", Message: "x"}}, "", "abc")
	line := ""
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "- `") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no warning line in:\n%s", body)
	}
	if strings.Count(line, "`") != 2 {
		t.Errorf("warning line has stray backticks: %q", line)
	}
	if strings.Contains(line, "&lt;") {
		t.Errorf("code span carries an entity, which renders literally: %q", line)
	}
}

func TestEmptyIsOnlyTrueWithNothingToSay(t *testing.T) {
	tests := []struct {
		name     string
		actions  []action.Action
		warnings []check.Warning
		want     bool
	}{
		{"nothing at all", nil, nil, true},
		// An approve is worth a green check run, not an email to every watcher.
		{"only an approve", []action.Action{{Verb: action.VerbApprove, Target: "pr"}}, nil, true},
		// A comment with no text cannot be performed and never reaches here, but
		// it must not be what makes a comment get posted either.
		{"a blank comment", []action.Action{{Verb: action.VerbComment, Target: "pr", Text: " "}}, nil, true},
		{"a finding", []action.Action{{Verb: action.VerbComment, Target: "pr", Text: "x"}}, nil, false},
		{"a warning", nil, []check.Warning{{Code: "tie"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Empty(tt.actions, tt.warnings); got != tt.want {
				t.Errorf("Empty = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolvedSaysTheFindingsAreGone(t *testing.T) {
	body := Resolved("abc123")
	if !strings.Contains(body, "no longer apply") {
		t.Errorf("resolved body does not say the findings are stale:\n%s", body)
	}
}

func TestPlanRendersWhatWouldDifferAndNothingElse(t *testing.T) {
	body := Plan(
		[]action.Action{{Verb: action.VerbBlock, Target: "pr", Text: "needs tests"}},
		[]action.Action{{Verb: action.VerbApprove, Target: "pr"}},
		"abc123",
	)
	for _, want := range []string{"Would additionally do", "Would no longer do", "block", "approve"} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "not performed") && !strings.Contains(body, "nothing below was performed") {
		t.Errorf("body does not say the diff is informational only:\n%s", body)
	}
}

func TestPlanWithOnlyOneSideOmitsTheOtherHeading(t *testing.T) {
	body := Plan([]action.Action{{Verb: action.VerbApprove, Target: "pr"}}, nil, "abc")
	if !strings.Contains(body, "Would additionally do") {
		t.Errorf("body should say what the head ruleset adds:\n%s", body)
	}
	if strings.Contains(body, "Would no longer do") {
		t.Errorf("body has nothing removed, should not claim it does:\n%s", body)
	}
}

func TestPlanResolvedSaysNoDifference(t *testing.T) {
	body := PlanResolved("abc123")
	if !strings.Contains(body, "no difference") {
		t.Errorf("plan-resolved body does not say the diff is gone:\n%s", body)
	}
}

// Every body has to be postable, which means non-empty and free of the marker
// the writer prepends.
func TestNoBodyCarriesItsOwnMarker(t *testing.T) {
	bodies := []string{
		Review([]action.Action{{Verb: action.VerbComment, Target: "pr", Text: "x"}}, nil, "s", "abc"),
		Review(nil, nil, "", ""),
		Broken("boom", "abc"),
		Resolved("abc"),
		Usage("try /review"),
	}
	for i, body := range bodies {
		if strings.TrimSpace(body) == "" {
			t.Errorf("body %d is empty", i)
		}
		for _, topic := range []string{TopicReview, TopicUsage} {
			if strings.Contains(body, Marker(topic)) {
				t.Errorf("body %d carries the %s marker:\n%s", i, topic, body)
			}
		}
	}
}
