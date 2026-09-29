package gitlab

import (
	"context"
	"fmt"

	"github.com/opentalon/talooner/internal/host"
)

type commitStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func statusOf(glStatus string) string {
	switch glStatus {
	case "pending", "created", "running":
		return host.StatusPending
	case "success":
		return host.ConclusionSuccess
	case "failed", "canceled":
		return host.ConclusionFailure
	default:
		return glStatus
	}
}

// CommitChecks maps onto GitLab's Commit Status API only: GitLab's Core-tier
// CI surface has no check-runs-with-annotations equivalent (see the
// GitLab-parity decisions in the workspace handoff notes), so Checks.Runs is
// always empty for this backend — only Checks.Statuses is populated.
func (c *Client) CommitChecks(ctx context.Context, owner, repo, sha string) (host.Checks, error) {
	if sha == "" {
		return host.Checks{}, fmt.Errorf("head sha is required to fetch checks")
	}
	path, err := projectPath(owner, repo, "repository", "commits", sha, "statuses")
	if err != nil {
		return host.Checks{}, err
	}

	all, err := paginate[commitStatus](ctx, c, path, nil)
	if err != nil {
		return host.Checks{}, fmt.Errorf("list commit statuses for %s/%s@%s: %w", owner, repo, sha, err)
	}

	var checks host.Checks
	for _, s := range all {
		checks.Statuses = append(checks.Statuses, host.CommitStatus{
			Context: s.Name,
			State:   statusOf(s.Status),
		})
	}
	return checks, nil
}
