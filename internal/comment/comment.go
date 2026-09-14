package comment

import (
	"fmt"
	"html"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/check"
)

const Version = "v1"

const (
	TopicReview  = "review"
	TopicUsage   = "usage"
	TopicVerdict = "verdict"
	TopicState   = "state"
	TopicPlan    = "plan"
)

func Marker(topic string) string {
	return "<!-- talooner:" + Version + ":" + topic + " -->"
}

func footer(sha string) string {
	if sha == "" {
		return "\n<sub>Talooner edits this comment in place; it always shows the current state.</sub>\n"
	}
	return fmt.Sprintf("\n<sub>Talooner reviewed <code>%s</code>. This comment is edited in place; it always shows the current state.</sub>\n",
		escape(short(sha)))
}

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

func Empty(actions []action.Action, warnings []check.Warning) bool {
	return len(comments(actions)) == 0 && len(warnings) == 0
}

func Broken(reason, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner could not review this pull request\n\n")
	b.WriteString(escape(strings.TrimSpace(reason)))
	b.WriteString("\n\nThis is a Talooner failure, not a policy outcome: the check run is neutral " +
		"rather than failing, so a broken bot does not block a merge.\n")
	b.WriteString(footer(sha))
	return b.String()
}

func NoRuleset(path, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner has nothing to review\n\n")
	fmt.Fprintf(&b, "No `%s` was found on the base branch, so there is no ruleset to evaluate against.\n", escape(path))
	b.WriteString(footer(sha))
	return b.String()
}

func Resolved(sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner review\n\n")
	b.WriteString("Nothing to report. The findings that were here no longer apply.\n")
	b.WriteString(footer(sha))
	return b.String()
}

func NoRulesFired(sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner review\n\n")
	b.WriteString("No rule in the ruleset matched this pull request. This is not the same " +
		"as reviewing it and finding nothing wrong — the ruleset has no opinion on a change " +
		"shaped like this one.\n")
	b.WriteString(footer(sha))
	return b.String()
}

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

func PlanResolved(sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner plan\n\n")
	b.WriteString("This pull request's own ruleset would make no difference to the base branch's decision.\n")
	b.WriteString(footer(sha))
	return b.String()
}

func PlanNow(actions []action.Action, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner plan\n\n")
	b.WriteString("This is what the head branch's own ruleset would decide right now, " +
		"evaluated with no writes.\n\n")

	findings := comments(actions)
	if len(findings) > 0 {
		b.WriteString("**Findings**\n\n")
		for _, a := range findings {
			b.WriteString(escape(strings.TrimSpace(a.Text)))
			b.WriteString("\n\n")
		}
	}

	if other := performed(actions); len(other) > 0 {
		b.WriteString("**Would also do**\n\n")
		for _, a := range other {
			fmt.Fprintf(&b, "- %s\n", escape(action.Describe(a)))
		}
		b.WriteString("\n")
	} else if len(findings) == 0 {
		b.WriteString("No rules fired.\n\n")
	}

	b.WriteString(footer(sha))
	return b.String()
}

func PlanNoRuleset(path, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner plan\n\n")
	fmt.Fprintf(&b, "No `%s` was found on the head branch, so there is nothing to plan.\n", escape(path))
	b.WriteString(footer(sha))
	return b.String()
}

func PlanBroken(reason, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner plan\n\n")
	b.WriteString(escape(strings.TrimSpace(reason)))
	b.WriteString("\n")
	b.WriteString(footer(sha))
	return b.String()
}

func Acknowledge() string {
	return "Evaluating this pull request…"
}

func Stopped() string {
	return "Unsubscribed. Talooner will not evaluate this pull request again until `!talooner /review` is run."
}

func Usage(text string) string {
	var b strings.Builder
	b.WriteString("### Talooner\n\n")
	b.WriteString(escape(strings.TrimSpace(text)))
	b.WriteString("\n")
	b.WriteString(footer(""))
	return b.String()
}

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

func WhyNotEvaluated(reason, sha string) string {
	var b strings.Builder
	b.WriteString("### Talooner explain\n\n")
	b.WriteString(escape(strings.TrimSpace(reason)))
	b.WriteString("\n")
	b.WriteString(footer(sha))
	return b.String()
}

func comments(actions []action.Action) []action.Action {
	var out []action.Action
	for _, a := range actions {
		if a.Verb == action.VerbComment && strings.TrimSpace(a.Text) != "" {
			out = append(out, a)
		}
	}
	return out
}

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

func escape(s string) string {
	return html.EscapeString(s)
}

func escapeCode(s string) string {
	return strings.NewReplacer("`", "", "\r", " ", "\n", " ").Replace(s)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
