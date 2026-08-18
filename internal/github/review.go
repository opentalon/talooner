package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Review events Talooner submits. COMMENT is deliberately absent: a review with
// no verdict is what the sticky comment is for, and it would cost every watcher
// an email for nothing.
const (
	ReviewApprove        = "APPROVE"
	ReviewRequestChanges = "REQUEST_CHANGES"
)

// Review states GitHub reports back. Only these two are still standing; a
// DISMISSED, COMMENTED or PENDING review is not a verdict anyone can act on, so
// none of them is Talooner's to dismiss.
const (
	StateApproved         = "APPROVED"
	StateChangesRequested = "CHANGES_REQUESTED"
)

// stateOf is the review state an event leaves behind, so a run can tell whether
// what it wants is already standing.
var stateOf = map[string]string{
	ReviewApprove:        StateApproved,
	ReviewRequestChanges: StateChangesRequested,
}

// Review is the verdict Talooner leaves as a GitHub review.
//
// Identity is the marker in the body, never a remembered id — the same choice
// StickyComment makes, and for the same reason: the id is not ours to keep
// between runs, and a maintainer may dismiss the review at any time.
type Review struct {
	// Marker is the HTML comment identifying Talooner's own reviews. It is
	// prepended to Body on the way out, so Body must not carry it.
	Marker string
	// Event is ReviewApprove, ReviewRequestChanges, or "" to retract: an empty
	// event dismisses whatever Talooner has standing and submits nothing. That
	// is the whole retraction half of actions.md, "Reversibility" — facts
	// retract, and a GitHub review does not unless something dismisses it.
	Event string
	Body  string
	// CommitID pins a submitted review to the sha it judged. Required whenever
	// Event is set: a review submitted against whatever HEAD happens to be is a
	// verdict on code nobody computed it from.
	CommitID string
	// DismissMessage is what GitHub shows in place of a dismissed review. It is
	// the audit trail for the retraction, so it says why.
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
		// The API rejects an empty dismissal message, and a dismissal with no
		// reason on it is one nobody can act on.
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

// text is what gets submitted: the marker, then the body.
func (rv Review) text() string { return rv.Marker + "\n" + rv.Body }

type reviewPayload struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

// SyncReview makes Talooner's standing review on the PR match rv, and returns
// the id of the review left standing — 0 when there is none, which is both the
// retraction case and the "nothing was owed" one.
//
// Everything here is about the retraction half:
//
//   - a verdict that flipped dismisses the old review before submitting the new
//     one, in that order. A dismissal that fails then leaves nothing standing
//     rather than an approval standing next to a request for changes, and an
//     approval is the permissive one to get wrong;
//   - a verdict that did not change leaves the review alone. Re-submitting the
//     same one at every push costs every reviewer an email and says nothing new;
//   - a review a human already dismissed reads as DISMISSED, so it is not ours
//     to dismiss again and retraction is a no-op rather than a 422.
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
			current = r.ID // already saying what this run wants to say
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
		return 0, fmt.Errorf("submit %s review on %s/%s#%d: %w", rv.Event, owner, repo, number, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("submit %s review on %s/%s#%d: response carried no id", rv.Event, owner, repo, number)
	}
	return written.ID, nil
}

// dismissReview retracts one review. A review that disappeared between the
// listing and the dismissal is not an error: something else already retracted
// it, which is the outcome this call wanted.
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

// findReviews returns Talooner's own standing reviews, oldest first. A listing
// that fails takes the call down rather than falling through to a submit, which
// is the duplicate — and the undismissed approval — this exists to prevent.
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
			continue // dismissed, or a plain comment: nothing standing to retract
		}
		mine = append(mine, r)
	}
	if len(mine) > 1 {
		c.log.Warn("more than one talooner review is standing, keeping one and dismissing the rest",
			"repo", owner+"/"+repo, "pr", number, "count", len(mine))
	}
	return mine, nil
}
