package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/opentalon/talooner/internal/host"
)

var ErrReviewPermission = errors.New(
	"TAL-E-REVIEW-PERM: GitLab rejected the review — the token needs Developer access (or higher) on this " +
		"project. Supply a PAT or project access token as GITLAB_TOKEN, see auth.md, \"Error codes\"")

var stateOf = map[string]string{
	host.ReviewApprove:        host.StateApproved,
	host.ReviewRequestChanges: host.StateChangesRequested,
}

func validateReview(rv host.Review) error {
	if strings.TrimSpace(rv.Marker) == "" {
		return errors.New("review needs a marker")
	}
	if strings.Contains(rv.Marker, "\n") {
		return fmt.Errorf("review marker %q spans lines", rv.Marker)
	}
	if strings.TrimSpace(rv.DismissMessage) == "" {
		return errors.New("review needs a dismissal message")
	}
	if rv.Event == "" {
		return nil
	}
	if _, ok := stateOf[rv.Event]; !ok {
		return fmt.Errorf("review event is %q, want APPROVE, REQUEST_CHANGES or empty", rv.Event)
	}
	if strings.TrimSpace(rv.Body) == "" {
		return fmt.Errorf("review with event %s needs a body", rv.Event)
	}
	if strings.Contains(rv.Body, rv.Marker) {
		return fmt.Errorf("review body carries its own marker")
	}
	if strings.TrimSpace(rv.CommitID) == "" {
		return fmt.Errorf("review with event %s needs a commit id", rv.Event)
	}
	return nil
}

func reviewText(rv host.Review) string { return rv.Marker + "\n" + rv.Body }

// verdict is a standing marker-bearing discussion thread: the GitLab analog
// of one of GitHub's review objects. Resolved stands in for GitHub's
// APPROVED state, unresolved for CHANGES_REQUESTED — GitLab has no
// REQUEST_CHANGES review state, so an unresolved thread is what blocks per
// the umbrella handoff's decision #5.
type verdict struct {
	discussionID string
	noteID       int64
	state        string
}

type discussionNote struct {
	ID         int64  `json:"id"`
	Body       string `json:"body"`
	System     bool   `json:"system"`
	Resolvable bool   `json:"resolvable"`
	Resolved   bool   `json:"resolved"`
}

type discussionPayload struct {
	ID    string           `json:"id"`
	Notes []discussionNote `json:"notes"`
}

func (c *Client) findVerdicts(ctx context.Context, owner, repo string, number int, marker string) ([]verdict, error) {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "discussions")
	if err != nil {
		return nil, err
	}
	all, err := paginate[discussionPayload](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list discussions on %s/%s!%d: %w", owner, repo, number, err)
	}

	var mine []verdict
	for _, d := range all {
		if len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		if first.ID == 0 || first.System || !strings.Contains(first.Body, marker) {
			continue
		}
		state := host.StateApproved
		if first.Resolvable && !first.Resolved {
			state = host.StateChangesRequested
		}
		mine = append(mine, verdict{discussionID: d.ID, noteID: first.ID, state: state})
	}
	if len(mine) > 1 {
		c.log.Warn("more than one talooner verdict thread is standing, keeping one and resolving the rest",
			"repo", owner+"/"+repo, "mr", number, "count", len(mine))
	}
	return mine, nil
}

func (c *Client) SyncReview(ctx context.Context, owner, repo string, number int, rv host.Review) (int64, error) {
	if number <= 0 {
		return 0, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if err := validateReview(rv); err != nil {
		return 0, err
	}

	standing, err := c.findVerdicts(ctx, owner, repo, number, rv.Marker)
	if err != nil {
		return 0, err
	}

	want := stateOf[rv.Event]
	var stale []verdict
	var current verdict
	for _, v := range standing {
		if rv.Event != "" && v.state == want && current.noteID == 0 {
			current = v
			continue
		}
		stale = append(stale, v)
	}

	for _, v := range stale {
		if err := c.dismissVerdict(ctx, owner, repo, number, v, rv.DismissMessage); err != nil {
			return 0, err
		}
	}

	if err := c.syncApproval(ctx, owner, repo, number, rv.Event, rv.CommitID); err != nil {
		return 0, err
	}

	if current.noteID != 0 {
		return current.noteID, nil
	}
	if rv.Event == "" {
		return 0, nil
	}
	return c.postVerdict(ctx, owner, repo, number, rv)
}

func (c *Client) postVerdict(ctx context.Context, owner, repo string, number int, rv host.Review) (int64, error) {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "discussions")
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: reviewText(rv)})
	if err != nil {
		return 0, fmt.Errorf("encode verdict for %s/%s!%d: %w", owner, repo, number, err)
	}

	var created discussionPayload
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &created); err != nil {
		return 0, fmt.Errorf("post %s verdict on %s/%s!%d: %w", rv.Event, owner, repo, number, err)
	}
	if len(created.Notes) == 0 || created.Notes[0].ID == 0 {
		return 0, fmt.Errorf("post %s verdict on %s/%s!%d: response carried no note id", rv.Event, owner, repo, number)
	}
	noteID := created.Notes[0].ID

	if rv.Event == host.ReviewApprove {
		if err := c.resolveDiscussion(ctx, owner, repo, number, created.ID, true); err != nil {
			return 0, err
		}
	}
	return noteID, nil
}

func (c *Client) dismissVerdict(ctx context.Context, owner, repo string, number int, v verdict, message string) error {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "discussions", v.discussionID, "notes")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: message})
	if err != nil {
		return fmt.Errorf("encode dismissal of verdict thread %s on %s/%s!%d: %w", v.discussionID, owner, repo, number, err)
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, nil); err != nil {
		if errors.Is(err, host.ErrNotFound) {
			c.log.Info("verdict thread disappeared before it could be dismissed",
				"repo", owner+"/"+repo, "mr", number, "discussion", v.discussionID)
			return nil
		}
		return fmt.Errorf("reply to verdict thread %s on %s/%s!%d: %w", v.discussionID, owner, repo, number, err)
	}
	return c.resolveDiscussion(ctx, owner, repo, number, v.discussionID, true)
}

func (c *Client) resolveDiscussion(ctx context.Context, owner, repo string, number int, discussionID string, resolved bool) error {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "discussions", discussionID)
	if err != nil {
		return err
	}
	q := url.Values{"resolved": {strconv.FormatBool(resolved)}}
	if _, err := c.do(ctx, request{method: http.MethodPut, path: path, query: q}, nil); err != nil {
		if errors.Is(err, host.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("resolve discussion %s on %s/%s!%d: %w", discussionID, owner, repo, number, err)
	}
	return nil
}

type approvalState struct {
	UserHasApproved bool `json:"user_has_approved"`
}

// syncApproval keeps GitLab's native approve/unapprove bit — a separate
// mechanism from the marker discussion above — pointed at the desired
// disposition. It is a no-op when the token's own approval already matches,
// so a re-run with nothing changed makes no write, same as the discussion
// side above.
func (c *Client) syncApproval(ctx context.Context, owner, repo string, number int, event, sha string) error {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "approvals")
	if err != nil {
		return err
	}
	var got approvalState
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &got); err != nil {
		return fmt.Errorf("read approval state of %s/%s!%d: %w", owner, repo, number, err)
	}

	want := event == host.ReviewApprove
	if got.UserHasApproved == want {
		return nil
	}

	verb := "unapprove"
	if want {
		verb = "approve"
	}
	apPath, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), verb)
	if err != nil {
		return err
	}
	var body []byte
	if want && sha != "" {
		body, err = json.Marshal(struct {
			SHA string `json:"sha"`
		}{SHA: sha})
		if err != nil {
			return fmt.Errorf("encode approve payload for %s/%s!%d: %w", owner, repo, number, err)
		}
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: apPath, body: body}, nil); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized:
				return fmt.Errorf("%s merge request %s/%s!%d: %w (%v)", verb, owner, repo, number, ErrReviewPermission, apiErr)
			case !want && apiErr.StatusCode == http.StatusNotFound:
				return nil
			}
		}
		return fmt.Errorf("%s merge request %s/%s!%d: %w", verb, owner, repo, number, err)
	}
	return nil
}
