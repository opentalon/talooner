package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	ReviewApprove        = "APPROVE"
	ReviewRequestChanges = "REQUEST_CHANGES"
)

var ErrReviewPermission = errors.New(
	"TAL-E-REVIEW-PERM: GitHub rejected the review — insufficient permissions for talooner. " +
		`Enable "Allow GitHub Actions to create and approve pull requests" for this repo or org ` +
		"(Settings → Actions → General), see auth.md, \"Error codes\"")

const (
	StateApproved         = "APPROVED"
	StateChangesRequested = "CHANGES_REQUESTED"
)

var stateOf = map[string]string{
	ReviewApprove:        StateApproved,
	ReviewRequestChanges: StateChangesRequested,
}

type Review struct {
	Marker         string
	Event          string
	Body           string
	CommitID       string
	DismissMessage string
}

func (rv Review) validate() error {
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

func (rv Review) text() string { return rv.Marker + "\n" + rv.Body }

type reviewPayload struct {
	ID       int64       `json:"id"`
	Body     string      `json:"body"`
	State    string      `json:"state"`
	CommitID string      `json:"commit_id"`
	User     *reviewUser `json:"user"`
}

type reviewUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ReviewReport struct {
	ID       int64
	Login    string
	Bot      bool
	State    string
	CommitID string
}

func (c *Client) PullRequestReviews(ctx context.Context, owner, repo string, number int) ([]ReviewReport, error) {
	if number <= 0 {
		return nil, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "reviews")
	if err != nil {
		return nil, err
	}
	all, err := paginate[reviewPayload](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list reviews on %s/%s#%d: %w", owner, repo, number, err)
	}

	out := make([]ReviewReport, 0, len(all))
	for _, r := range all {
		if r.ID == 0 {
			continue
		}
		rr := ReviewReport{ID: r.ID, State: r.State, CommitID: r.CommitID}
		if r.User != nil {
			rr.Login = r.User.Login
			rr.Bot = r.User.Type == "Bot"
		}
		out = append(out, rr)
	}
	return out, nil
}

func (c *Client) SyncReview(ctx context.Context, owner, repo string, number int, rv Review) (int64, error) {
	if number <= 0 {
		return 0, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if err := rv.validate(); err != nil {
		return 0, err
	}

	standing, err := c.findReviews(ctx, owner, repo, number, rv.Marker)
	if err != nil {
		return 0, err
	}

	want := stateOf[rv.Event]
	var stale []reviewPayload
	var current int64
	for _, r := range standing {
		if rv.Event != "" && r.State == want && current == 0 {
			current = r.ID
			continue
		}
		stale = append(stale, r)
	}
	if current != 0 && len(stale) == 0 {
		return current, nil
	}

	for _, r := range stale {
		if err := c.dismissReview(ctx, owner, repo, number, r.ID, rv.DismissMessage); err != nil {
			return 0, err
		}
	}
	if current != 0 || rv.Event == "" {
		return current, nil
	}

	raw, err := json.Marshal(struct {
		CommitID string `json:"commit_id"`
		Body     string `json:"body"`
		Event    string `json:"event"`
	}{CommitID: rv.CommitID, Body: rv.text(), Event: rv.Event})
	if err != nil {
		return 0, fmt.Errorf("encode review for %s/%s#%d: %w", owner, repo, number, err)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "reviews")
	if err != nil {
		return 0, err
	}
	var written reviewPayload
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &written); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnprocessableEntity) {
			return 0, fmt.Errorf("submit %s review on %s/%s#%d: %w (%v)", rv.Event, owner, repo, number, ErrReviewPermission, apiErr)
		}
		return 0, fmt.Errorf("submit %s review on %s/%s#%d: %w", rv.Event, owner, repo, number, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("submit %s review on %s/%s#%d: response carried no id", rv.Event, owner, repo, number)
	}
	return written.ID, nil
}

func (c *Client) dismissReview(ctx context.Context, owner, repo string, number int, id int64, message string) error {
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "reviews", fmt.Sprint(id), "dismissals")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Message string `json:"message"`
		Event   string `json:"event"`
	}{Message: message, Event: "DISMISS"})
	if err != nil {
		return fmt.Errorf("encode dismissal of review %d on %s/%s#%d: %w", id, owner, repo, number, err)
	}
	if _, err := c.do(ctx, request{method: http.MethodPut, path: path, body: raw}, nil); err != nil {
		if errors.Is(err, ErrNotFound) {
			c.log.Info("review disappeared before it could be dismissed",
				"repo", owner+"/"+repo, "pr", number, "review", id)
			return nil
		}
		return fmt.Errorf("dismiss review %d on %s/%s#%d: %w", id, owner, repo, number, err)
	}
	return nil
}

func (c *Client) findReviews(ctx context.Context, owner, repo string, number int, marker string) ([]reviewPayload, error) {
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "reviews")
	if err != nil {
		return nil, err
	}
	all, err := paginate[reviewPayload](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list reviews on %s/%s#%d: %w", owner, repo, number, err)
	}

	var mine []reviewPayload
	for _, r := range all {
		if r.ID == 0 || !strings.Contains(r.Body, marker) {
			continue
		}
		if r.State != StateApproved && r.State != StateChangesRequested {
			continue
		}
		mine = append(mine, r)
	}
	if len(mine) > 1 {
		c.log.Warn("more than one talooner review is standing, keeping one and dismissing the rest",
			"repo", owner+"/"+repo, "pr", number, "count", len(mine))
	}
	return mine, nil
}
