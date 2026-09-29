package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/opentalon/talooner/internal/host"
)

const maxStatusFieldRunes = 255

// GitLab's commit status has no "neutral" state (unlike GitHub's Checks
// API, which this vocabulary was borrowed from) — every backend's
// ConclusionNeutral maps to "success" here so a broken-bot check
// (internal/check.Broken's explicit "must not block a merge" contract)
// never reads as a failing, potentially merge-blocking status on GitLab.
func commitStatusOf(conclusion string) (string, error) {
	switch conclusion {
	case host.ConclusionSuccess, host.ConclusionNeutral:
		return "success", nil
	case host.ConclusionFailure:
		return "failed", nil
	default:
		return "", fmt.Errorf("conclusion %q has no gitlab commit-status equivalent", conclusion)
	}
}

func truncateField(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	keep := []rune(s)[:max-1]
	return string(keep) + "…"
}

// statusDescription folds the check run's summary into the 255-byte
// description GitLab's commit status API allows — there is no long-form
// output surface (like GitHub Checks' Output.Text) to hold the rest, and
// annotations (line-level detail) have no GitLab equivalent at all (see
// the GitLab-parity decisions in the workspace handoff notes), so they are
// folded down to a count rather than dropped silently.
func statusDescription(cr host.CheckRun) string {
	desc := strings.TrimSpace(cr.Title)
	if first := firstLine(cr.Summary); first != "" && first != desc {
		if desc != "" {
			desc += ": "
		}
		desc += first
	}
	if n := len(cr.Annotations); n > 0 {
		desc += fmt.Sprintf(" (%d annotation(s))", n)
	}
	return truncateField(desc, maxStatusFieldRunes)
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

type commitStatusWrite struct {
	State       string `json:"state"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

func (c *Client) UpsertCheckRun(ctx context.Context, owner, repo string, cr host.CheckRun) (int64, error) {
	if strings.TrimSpace(cr.Name) == "" {
		return 0, fmt.Errorf("check run needs a name")
	}
	if strings.TrimSpace(cr.HeadSHA) == "" {
		return 0, fmt.Errorf("check run %s needs a head sha", cr.Name)
	}
	state, err := commitStatusOf(cr.Conclusion)
	if err != nil {
		return 0, fmt.Errorf("check run %s: %w", cr.Name, err)
	}

	body := commitStatusWrite{
		State:       state,
		Name:        cr.Name,
		Description: statusDescription(cr),
		TargetURL:   truncateField(cr.DetailsURL, maxStatusFieldRunes),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("encode check run %s for %s/%s@%s: %w", cr.Name, owner, repo, cr.HeadSHA, err)
	}
	path, err := projectPath(owner, repo, "statuses", cr.HeadSHA)
	if err != nil {
		return 0, err
	}

	var written struct {
		ID int64 `json:"id"`
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &written); err != nil {
		return 0, fmt.Errorf("write check run %s on %s/%s@%s: %w", cr.Name, owner, repo, cr.HeadSHA, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("write check run %s on %s/%s@%s: response carried no id", cr.Name, owner, repo, cr.HeadSHA)
	}
	return written.ID, nil
}
