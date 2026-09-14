package check

import (
	"fmt"
	"strings"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/github"
)

const Name = "talooner"

const maxAnnotations = 50

type Warning struct {
	Code    string
	Message string
}

type Diagnostic struct {
	Path    string
	Line    int
	Column  int
	Message string
}

func Decision(actions []action.Action, warnings []Warning, summary string, unitCount int) github.CheckRun {
	var blocked, approved bool
	for _, a := range actions {
		switch a.Verb {
		case action.VerbBlock:
			blocked = true
		case action.VerbApprove:
			approved = true
		}
	}

	cr := github.CheckRun{Name: Name}
	switch {
	case blocked:
		cr.Conclusion = github.ConclusionFailure
		cr.Title = "Changes requested"
	case approved:
		cr.Conclusion = github.ConclusionSuccess
		cr.Title = "Approved"
	case len(actions) > 0:
		cr.Conclusion = github.ConclusionNeutral
		cr.Title = "Reviewed"
	default:
		cr.Conclusion = github.ConclusionSuccess
		cr.Title = "No issues found"
	}

	var b strings.Builder
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if len(actions) == 0 {
		b.WriteString("No rule matched this pull request — nothing to flag.\n")
	} else {
		b.WriteString("Actions:\n\n")
		for _, a := range actions {
			fmt.Fprintf(&b, "- %s\n", action.Describe(a))
		}
	}
	if blocked && approved {
		b.WriteString("\nBoth `approve` and `block` fired and the ruleset does not say which wins. " +
			"The check is reported as a failure until the tie is resolved with `overrides` or `priority`.\n")
	}
	if unitCount > 0 {
		unit := "code units"
		if unitCount == 1 {
			unit = "code unit"
		}
		fmt.Fprintf(&b, "\n%d %s evaluated for documented-behavior drift.\n", unitCount, unit)
	}
	writeWarnings(&b, warnings)

	cr.Summary = b.String()
	return cr
}

func Broken(reason string, diags []Diagnostic) github.CheckRun {
	cr := github.CheckRun{
		Name:       Name,
		Conclusion: github.ConclusionNeutral,
		Title:      "Talooner could not review this pull request",
	}

	var b strings.Builder
	b.WriteString(strings.TrimSpace(reason))
	b.WriteString("\n\nThis is a Talooner failure, not a policy outcome, so the check is neutral " +
		"rather than failing — a broken bot must not block a merge.\n")

	kept := diags
	if len(kept) > maxAnnotations {
		kept = kept[:maxAnnotations]
	}
	for _, d := range kept {
		line := max(d.Line, 1)
		cr.Annotations = append(cr.Annotations, github.Annotation{
			Path:      d.Path,
			StartLine: line,
			EndLine:   line,
			Level:     github.LevelFailure,
			Title:     "Ruleset error",
			Message:   annotationMessage(d),
		})
	}
	if n := len(diags) - len(kept); n > 0 {
		fmt.Fprintf(&b, "\n%d further diagnostic(s) are not annotated; fix these first.\n", n)
	}

	cr.Summary = b.String()
	return cr
}

func annotationMessage(d Diagnostic) string {
	msg := strings.TrimSpace(d.Message)
	if msg == "" {
		msg = "ruleset error"
	}
	if d.Column > 0 {
		return fmt.Sprintf("column %d: %s", d.Column, msg)
	}
	return msg
}

func writeWarnings(b *strings.Builder, warnings []Warning) {
	if len(warnings) == 0 {
		return
	}
	b.WriteString("\nWarnings:\n\n")
	for _, w := range warnings {
		switch {
		case w.Code != "" && w.Message != "":
			fmt.Fprintf(b, "- `%s`: %s\n", w.Code, w.Message)
		case w.Code != "":
			fmt.Fprintf(b, "- `%s`\n", w.Code)
		default:
			fmt.Fprintf(b, "- %s\n", w.Message)
		}
	}
}
