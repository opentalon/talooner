// Package check turns a decision into the one check run Talooner writes. It is
// pure: it decides what the check run says, and github writes it.
//
// The mapping is actions.md, "Check run":
//
//	a block fired                        → failure
//	an approve fired, no block           → success
//	rules fired, none decisive           → neutral
//	Talooner itself broke                → neutral, plus annotations
//
// That last row is the whole point of the package. A repo that marks the
// talooner check required has handed Talooner a veto over its merges; a bot
// that turns its own bugs into red checks abuses that. So: fail closed on
// policy outcomes, fail open on Talooner's own faults. Broken never returns
// failure, and there is no path here that lets it.
package check

import (
	"fmt"
	"strings"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/github"
)

// Name is the check run's name, and with the head sha its identity. It is
// stable across versions on purpose: a repo puts this string in its branch
// protection, so renaming it silently un-gates every repo that did.
const Name = "talooner"

// maxAnnotations caps what one check run carries. Past that the summary says
// how many were left out, because a truncated list presented as complete is the
// failure this package exists to avoid.
const maxAnnotations = 50

// Warning is a non-fatal condition the plugin surfaced with a decision — an
// unresolved approve/block tie, most importantly.
type Warning struct {
	Code    string
	Message string
}

// Diagnostic is one positioned finding about the ruleset, from
// validate_ruleset. Line 0 means the compiler could not place it.
type Diagnostic struct {
	Path    string
	Line    int
	Column  int
	Message string
}

// Decision renders the check run for an evaluation that completed. actions is
// the decoded action set, warnings what the plugin surfaced alongside it, and
// summary the explain summary, which may be empty.
func Decision(actions []action.Action, warnings []Warning, summary string) github.CheckRun {
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
		// block-wins is the last-resort tiebreak for a tie the plugin could not
		// resolve, not a conflict rule of its own: the plugin's defeasible
		// machinery decides, and both actions arriving means it could not. The
		// warning below is the product; this just gives the check one value
		// (actions.md, "Conflict resolution happens plugin-side").
		cr.Conclusion = github.ConclusionFailure
		cr.Title = "Changes requested"
	case approved:
		cr.Conclusion = github.ConclusionSuccess
		cr.Title = "Approved"
	case len(actions) > 0:
		cr.Conclusion = github.ConclusionNeutral
		cr.Title = "Reviewed"
	default:
		// No action firing is a verdict too, not a missing one. Saying so is
		// what distinguishes it from a run that never happened.
		cr.Conclusion = github.ConclusionNeutral
		cr.Title = "No rules fired"
	}

	var b strings.Builder
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	if len(actions) == 0 {
		b.WriteString("No rule matched this pull request.\n")
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
	writeWarnings(&b, warnings)

	cr.Summary = b.String()
	return cr
}

// Broken renders the check run for a run that failed for Talooner's own
// reasons: a ruleset that would not compile, an extraction that fell over, a
// cluster that could not be reached after the PR was fetched.
//
// The conclusion is always neutral. reason is one line naming what broke;
// diags, when there are any, become annotations pinned to the offending lines.
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
		line := max(d.Line, 1) // an unplaceable diagnostic still points at the file
		cr.Annotations = append(cr.Annotations, github.Annotation{
			Path:      d.Path,
			StartLine: line,
			EndLine:   line,
			// Failure level is display only: the annotation is loud, the check
			// run stays neutral.
			Level:   github.LevelFailure,
			Title:   "Ruleset error",
			Message: annotationMessage(d),
		})
	}
	if n := len(diags) - len(kept); n > 0 {
		fmt.Fprintf(&b, "\n%d further diagnostic(s) are not annotated; fix these first.\n", n)
	}

	cr.Summary = b.String()
	return cr
}

// annotationMessage keeps the column in the text, since an annotation can only
// point at a line.
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
