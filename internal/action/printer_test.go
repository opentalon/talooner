package action

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPrintingCoversTheWholeVocabulary(t *testing.T) {
	if err := Printing(&bytes.Buffer{}).Complete(); err != nil {
		t.Fatalf("Printing registry is incomplete: %v", err)
	}
}

func TestPrinterRendersOneLinePerAction(t *testing.T) {
	var buf bytes.Buffer
	actions := []Action{
		{Verb: VerbBlock, Target: "pr.merge"},
		{Verb: VerbComment, Target: "pr", Text: "no description\nand a long body"},
		{Verb: VerbAssign, Target: "pr", Assignee: "@alice"},
		{Verb: VerbRequire, Target: "review.security"},
		{Verb: VerbNotify, Target: "#eng", Text: "blocked"},
		{Verb: VerbEmit, Name: "needs_design"},
		{Verb: VerbApprove, Target: "pr"},
	}
	if err := Printing(&buf).Execute(context.Background(), actions); err != nil {
		t.Fatalf("Execute returned %v, want no error", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	want := []string{
		"block pr.merge",
		"comment on pr: no description",
		"assign pr to @alice",
		"require review.security",
		"notify #eng: blocked",
		"emit event.needs_design",
		"approve pr",
	}
	if len(lines) != len(want) {
		t.Fatalf("printer wrote %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// A plan that lost a line is a plan that lies about what a run would do, so a
// failing write fails the plan.
func TestPrinterReportsAFailedWrite(t *testing.T) {
	err := Printing(brokenWriter{}).Execute(context.Background(), []Action{{Verb: VerbApprove, Target: "pr"}})
	if err == nil {
		t.Fatal("Execute returned no error, want the write failure")
	}
	if !strings.Contains(err.Error(), "disk is on fire") {
		t.Errorf("Execute error = %q, want the underlying write failure", err)
	}
}

// The printer validates through the same registry the real run uses, so an
// unperformable action is caught in a plan rather than at execution time.
func TestPrinterRejectsWhatExecutionWouldReject(t *testing.T) {
	var buf bytes.Buffer
	err := Printing(&buf).Execute(context.Background(), []Action{{Verb: VerbAssign, Target: "pr"}})
	if !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("Execute returned %v, want ErrInvalidAction", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printer wrote %q, want nothing", buf.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("disk is on fire") }
