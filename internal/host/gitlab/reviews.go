package gitlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/opentalon/talooner/internal/host"
)

const (
	noteApproved   = "approved this merge request"
	noteUnapproved = "unapproved this merge request"
)

// PullRequestReviews has no direct GitLab equivalent of GitHub's review list:
// GitLab folds approval into system notes ("approved/unapproved this merge
// request") and has no formal "request changes" action at all (Core tier),
// so an unresolved, resolvable discussion thread's opening note stands in
// for a REQUEST_CHANGES review — the same emulation host.go's GitLab-parity
// decisions use for Talooner's own standing verdict, extended here to every
// reviewer's threads, not just the bot's marker thread.
//
// GitLab's approval system notes carry no commit sha, so every active
// approval is stamped with the merge request's current head sha rather than
// the sha that was actually current when the approval was given.
func (c *Client) PullRequestReviews(ctx context.Context, owner, repo string, number int) ([]host.ReviewReport, error) {
	if number <= 0 {
		return nil, fmt.Errorf("merge request iid must be positive, got %d", number)
	}

	pr, err := c.PullRequest(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("resolve head sha for %s/%s!%d: %w", owner, repo, number, err)
	}

	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "discussions")
	if err != nil {
		return nil, err
	}
	all, err := paginate[discussionPayload](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list discussions on %s/%s!%d: %w", owner, repo, number, err)
	}

	var out []host.ReviewReport
	for _, d := range all {
		if len(d.Notes) == 0 {
			continue
		}
		if rr, ok := reviewReportOf(d.Notes[0], pr.HeadSHA); ok {
			out = append(out, rr)
		}
	}
	return out, nil
}

func reviewReportOf(n discussionNote, headSHA string) (host.ReviewReport, bool) {
	if n.ID == 0 {
		return host.ReviewReport{}, false
	}
	var login string
	var bot bool
	if n.Author != nil {
		login, bot = n.Author.Username, n.Author.Bot
	}

	if n.System {
		switch strings.TrimSpace(n.Body) {
		case noteApproved:
			return host.ReviewReport{ID: n.ID, Login: login, Bot: bot, State: host.StateApproved, CommitID: headSHA}, true
		case noteUnapproved:
			return host.ReviewReport{ID: n.ID, Login: login, Bot: bot, State: "DISMISSED"}, true
		default:
			return host.ReviewReport{}, false
		}
	}

	if n.Resolvable && !n.Resolved {
		return host.ReviewReport{ID: n.ID, Login: login, Bot: bot, State: host.StateChangesRequested}, true
	}
	return host.ReviewReport{}, false
}
