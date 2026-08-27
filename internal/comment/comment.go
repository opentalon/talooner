// Package comment renders the sticky comments Talooner writes on a pull
// request. It is pure: it decides what a comment says, and github writes it.
//
// One comment per logical topic, identified by an HTML marker, edited in place
// on every run (actions.md, "Sticky comments"). A PR with thirty pushes carries
// one review comment showing the current state, not thirty showing its history.
// A topic whose condition no longer holds is edited to Resolved rather than
// deleted — the edit history is the audit trail, and a deleted comment takes
// the replies under it with it.
//
// Two properties this package exists to hold:
//
//   - Plugin-supplied text is escaped. It reaches here interpolated from facts,
//     and on a fork PR those facts are the title, body and branch name of a
//     stranger's branch. Escaped means it cannot inject HTML, and in particular
//     cannot forge a marker and take over a topic on the next run.
//   - The body is derived from the whole action set, like the check run, rather
//     than performed one action at a time. `do comment` firing three times is
//     one comment with three findings, which is what "one comment per topic"
//     means.
package comment

import (
	"fmt"
	"html"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/check"
)

// Version is the marker's schema version. It is part of every marker so a
// future format can find and retire the comments this one wrote.
const Version = "v1"

// Topics. Each is one marker, and one comment on the PR.
const (
	// TopicReview is the verdict: findings, what Talooner did, why the run
	// could not produce a verdict at all. One per PR, current state only.
	TopicReview = "review"
	// TopicUsage is the reply to a command Talooner did not understand. It is
	// its own topic so a typo does not overwrite the verdict.
	TopicUsage = "usage"
	// TopicVerdict marks the body of the GitHub review Talooner submits, not a
	// comment. It is a topic all the same: it is how the next run finds the
	// review it left, so it has to be spelled the way every other marker is.
	TopicVerdict = "verdict"
	// TopicState is the assignment ledger: the assignees and review requests
	// Talooner itself added, so a later run can take back its own and only its
	// own. GitHub reports an assignee a human added and one Talooner added
	// identically, so without this comment there is no way to retract one
	// without also taking away somebody's deliberate act.
	TopicState = "state"
	// TopicPlan is the fork-PR decision diff (E2, #21): what a fork's own
	// head-branch ruleset would do differently from the base ruleset that
	// actually governs writes. It is its own topic so a rule change under
	// review does not overwrite the verdict the base ruleset already produced.
	TopicPlan = "plan"
)

// Marker is the HTML comment identifying a topic. It is what makes a comment
// findable on the next run, so its spelling is a compatibility surface.
func Marker(topic string) string {
	return "<!-- talooner:" + Version + ":" + topic + " -->"
}

// footer closes every body: which sha it describes, and the fact that it is
// edited rather than reposted, so nobody goes looking for the older ones.
func footer(sha string) string {
	if sha == "" {
		return "\n<sub>Talooner edits this comment in place; it always shows the current state.</sub>\n"
	}
	return fmt.Sprintf("\n<sub>Talooner reviewed <code>%s</code>. This comment is edited in place; it always shows the current state.</sub>\n",
		escape(short(sha)))
}

// Review is the verdict comment: the findings `do comment` produced, what else
// Talooner did, and anything the plugin warned about.
//
// It is written only when there is something to say — Empty reports that — so
// a PR whose rules all passed quietly gets a check run and no comment.
func Review(actions []action.Action, warnings []check.Warning, summary, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner review\n\n")
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString(escape(s))
		b.WriteString("\n\n")
	}

	findings := comments(actions)
	if len(findings) > 0 {
		b.WriteString("**Findings**\n\n")
		for _, a := range findings {
			// The text is markdown authored by the ruleset and interpolated
			// from facts, so it is escaped and given its own paragraph rather
			// than being folded into a bullet, where a newline in it would
			// break the list.
			b.WriteString(escape(strings.TrimSpace(a.Text)))
			b.WriteString("\n\n")
		}
	}

	if other := performed(actions); len(other) > 0 {
		b.WriteString("**Also**\n\n")
		for _, a := range other {
			fmt.Fprintf(&b, "- %s\n", escape(action.Describe(a)))
		}
		b.WriteString("\n")
	}

	writeWarnings(&b, warnings)
	b.WriteString(footer(sha))
	return b.String()
}

// Empty reports whether a Review would carry nothing worth notifying the PR's
// watchers about. The check run is the machine-readable half and is always
// written; a comment costs everyone an email, so it is not.
func Empty(actions []action.Action, warnings []check.Warning) bool {
	return len(comments(actions)) == 0 && len(warnings) == 0
}

// Broken is the review comment for a run that could not produce a verdict —
// a ruleset that will not compile, most of the time. It shares the review topic
// on purpose: it is the current state of the same question, so it replaces the
// findings of the run before it rather than sitting next to them.
func Broken(reason, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner could not review this pull request\n\n")
	b.WriteString(escape(strings.TrimSpace(reason)))
	b.WriteString("\n\nThis is a Talooner failure, not a policy outcome: the check run is neutral " +
		"rather than failing, so a broken bot does not block a merge.\n")
	b.WriteString(footer(sha))
	return b.String()
}

// NoRuleset is the review comment for a repo that has not onboarded: no
// rules.tln was found on the base branch (E1, #20). It is deliberately not
// paired with a check run — a talooner check on a repo that never asked for one
// is noise (D2) — so this comment is the only trace of the run.
func NoRuleset(path, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner has nothing to review\n\n")
	fmt.Fprintf(&b, "No `%s` was found on the base branch, so there is no ruleset to evaluate against.\n", escape(path))
	b.WriteString(footer(sha))
	return b.String()
}

// Resolved is what a review comment is edited to once none of its findings
// apply any more. Never deleted: the thread under it is somebody's discussion,
// and the edit history is the audit trail (actions.md, "Reversibility").
func Resolved(sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner review\n\n")
	b.WriteString("Nothing to report. The findings that were here no longer apply.\n")
	b.WriteString(footer(sha))
	return b.String()
}

// Plan is the fork-PR decision diff (E2, #21): what this PR's own head-branch
// ruleset would do differently from the base ruleset that actually governs
// writes. Nothing in added or removed was performed — the base decision
// already was, separately, before this comment is ever written.
func Plan(added, removed []action.Action, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner plan\n\n")
	b.WriteString("This pull request's own `.github/talooner/rules.tln` was evaluated for " +
		"comparison only. The base branch's ruleset is what governs writes " +
		"(architecture.md, \"Fork safety\"); nothing below was performed.\n\n")
	if len(added) > 0 {
		b.WriteString("**Would additionally do**\n\n")
		for _, a := range added {
			fmt.Fprintf(&b, "- %s\n", escape(action.Describe(a)))
		}
		b.WriteString("\n")
	}
	if len(removed) > 0 {
		b.WriteString("**Would no longer do**\n\n")
		for _, a := range removed {
			fmt.Fprintf(&b, "- %s\n", escape(action.Describe(a)))
		}
		b.WriteString("\n")
	}
	b.WriteString(footer(sha))
	return b.String()
}

// PlanResolved is what the plan comment is edited to once the head branch's
// ruleset no longer decides anything differently from the base branch's — or
// once the head branch no longer carries a ruleset of its own at all. Never
// deleted, same reasoning as Resolved: the edit history is the audit trail.
func PlanResolved(sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner plan\n\n")
	b.WriteString("This pull request's own ruleset would make no difference to the base branch's decision.\n")
	b.WriteString(footer(sha))
	return b.String()
}

// Usage is the one reply a command Talooner does not understand gets. The
// caller has already established that the commander has write access; replying
// to anyone else advertises the bot and hands them a way to make it post
// (command.Authorize).
func Usage(text string) string {
	var b strings.Builder
	b.WriteString("### Talooner\n\n")
	b.WriteString(escape(strings.TrimSpace(text)))
	b.WriteString("\n")
	b.WriteString(footer(""))
	return b.String()
}

// Why is the reply to `/why`: the plugin's persisted explanation for the
// decision at sha (cluster.Client.ExplainPR). It is posted as a plain
// comment, never edited — a later `/why` at a later sha is a different
// question, not an update to this answer, unlike the ongoing verdict Review
// keeps current in place.
func Why(explain *taloonerpb.Explain, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner explain\n\n")
	if s := strings.TrimSpace(explain.GetSummary()); s != "" {
		b.WriteString(escape(s))
		b.WriteString("\n\n")
	}

	if firings := explain.GetFirings(); len(firings) > 0 {
		b.WriteString("**Rules**\n\n")
		for _, f := range firings {
			line := fmt.Sprintf("`%s`", escapeCode(f.GetRule()))
			if p := f.GetPriority(); p != "" {
				line += fmt.Sprintf(" (%s)", escape(p))
			}
			if f.GetStrict() {
				line += ", strict"
			}
			if f.GetDefeated() {
				line += ", defeated"
			}
			if overrides := f.GetOverrides(); len(overrides) > 0 {
				line += fmt.Sprintf(" — overrides %s", escape(strings.Join(overrides, ", ")))
			}
			fmt.Fprintf(&b, "- %s\n", line)
		}
		b.WriteString("\n")
	}
	b.WriteString(footer(sha))
	return b.String()
}

// WhyNotEvaluated is the reply to `/why` when the plugin has no decision on
// record for this PR's current head sha — a distinct, clear answer, not an
// empty explanation that would read like "no rules fired".
func WhyNotEvaluated(reason, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner explain\n\n")
	b.WriteString(escape(strings.TrimSpace(reason)))
	b.WriteString("\n")
	b.WriteString(footer(sha))
	return b.String()
}

// comments is the actions that produced human-readable findings.
func comments(actions []action.Action) []action.Action {
	var out []action.Action
	for _, a := range actions {
		if a.Verb == action.VerbComment && strings.TrimSpace(a.Text) != "" {
			out = append(out, a)
		}
	}
	return out
}

// performed is everything else Talooner did, rendered as one line each so the
// comment is a complete account of the run and not only its prose half.
func performed(actions []action.Action) []action.Action {
	var out []action.Action
	for _, a := range actions {
		if a.Verb != action.VerbComment {
			out = append(out, a)
		}
	}
	return out
}

func writeWarnings(b *strings.Builder, warnings []check.Warning) {
	if len(warnings) == 0 {
		return
	}
	b.WriteString("**Warnings**\n\n")
	for _, w := range warnings {
		switch {
		case w.Code != "" && w.Message != "":
			fmt.Fprintf(b, "- `%s`: %s\n", escapeCode(w.Code), escape(w.Message))
		case w.Code != "":
			fmt.Fprintf(b, "- `%s`\n", escapeCode(w.Code))
		default:
			fmt.Fprintf(b, "- %s\n", escape(w.Message))
		}
	}
	b.WriteString("\n")
}

// escape renders text as text. Everything in a comment body except this
// package's own scaffolding goes through it.
//
// It escapes the HTML metacharacters, which is what closes the two holes that
// matter: raw HTML, and an HTML comment forging `<!-- talooner:v1:review -->`
// to make the next run edit an attacker's comment instead of Talooner's.
// Markdown emphasis and links survive, because markdown cannot execute
// anything and GitHub sanitises what it renders.
func escape(s string) string {
	return html.EscapeString(s)
}

// escapeCode is escape's counterpart for text going inside a code span, where
// HTML entities are shown literally rather than decoded and markdown already
// neutralises everything except the fence itself. So: drop the backticks and
// the newlines that would end the span, and leave the rest alone.
func escapeCode(s string) string {
	return strings.NewReplacer("`", "", "\r", " ", "\n", " ").Replace(s)
}

// short is the abbreviated sha a human reads.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
