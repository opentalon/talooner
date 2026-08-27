package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// maxCommentBytes is GitHub's cap on an issue comment body. A body over it is
// rejected outright, so the writer truncates rather than losing the comment: a
// findings list that got long is exactly when the run must still say something.
const maxCommentBytes = 65536

// truncationNotice replaces what was cut. It is counted against the cap, so the
// truncated body always fits.
const truncationNotice = "\n\n… truncated: this comment hit GitHub's size limit. The check run carries the full verdict.\n"

// StickyComment is one comment Talooner owns on a pull request, identified by
// an HTML marker rather than by its id: the id is not ours to keep between
// runs, and a maintainer may delete the comment at any time.
//
// One marker is one logical topic. Re-running edits the comment carrying the
// marker rather than adding another, so a PR with thirty pushes shows one
// comment with the current state (actions.md, "Sticky comments").
type StickyComment struct {
	// Marker is the HTML comment identifying the topic, e.g.
	// "<!-- talooner:v1:review -->". It is prepended to Body on the way out, so
	// Body must not carry it.
	Marker string
	Body   string
	// EditOnly writes nothing when no comment carries the marker yet. It is how
	// a topic is retired: a condition that no longer holds edits its comment to
	// a resolved state, and posts nothing on a PR that never had one
	// (actions.md, "Reversibility" — comments are never deleted).
	EditOnly bool
}

func (s StickyComment) validate() error {
	if strings.TrimSpace(s.Marker) == "" {
		return errors.New("sticky comment needs a marker")
	}
	if strings.Contains(s.Marker, "\n") {
		return fmt.Errorf("sticky comment marker %q spans lines", s.Marker)
	}
	if strings.TrimSpace(s.Body) == "" {
		// A comment saying nothing still notifies everyone watching the PR.
		return fmt.Errorf("sticky comment %s needs a body", s.Marker)
	}
	if strings.Contains(s.Body, s.Marker) {
		return fmt.Errorf("sticky comment %s carries its own marker in the body", s.Marker)
	}
	return nil
}

// text is what actually gets posted: the marker, then the body, truncated to
// the API's limit if it has to be.
func (s StickyComment) text() string {
	return truncate(s.Marker + "\n" + s.Body)
}

// truncate caps s at GitHub's comment size limit, appending truncationNotice
// so a body that had to be cut still says so.
func truncate(s string) string {
	if len(s) <= maxCommentBytes {
		return s
	}
	keep := maxCommentBytes - len(truncationNotice)
	s = s[:max(keep, 0)]
	// Cutting mid-rune would post invalid UTF-8; back off to the last whole one.
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + truncationNotice
}

type issueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// UpsertComment writes s on pull request number, editing the comment that
// already carries the marker instead of posting a second one. It returns the
// comment's id, or 0 when EditOnly found nothing to edit.
//
// The unhappy paths are the point:
//
//   - the marker comment was deleted by a maintainer: it is simply not in the
//     listing, so this posts a new one rather than 404ing on a remembered id;
//   - two comments carry the marker, from a botched earlier run: the oldest is
//     edited, every time, and nothing fans out. Oldest rather than newest
//     because it is the one with the PR's history under it;
//   - a listing that fails takes the call down instead of falling through to a
//     create, which is the duplicate this exists to prevent.
func (c *Client) UpsertComment(ctx context.Context, owner, repo string, number int, s StickyComment) (int64, error) {
	if number <= 0 {
		return 0, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if err := s.validate(); err != nil {
		return 0, err
	}

	id, _, err := c.findComment(ctx, owner, repo, number, s.Marker)
	if err != nil {
		return 0, err
	}
	if id == 0 && s.EditOnly {
		return 0, nil
	}

	raw, err := json.Marshal(issueComment{Body: s.text()})
	if err != nil {
		return 0, fmt.Errorf("encode comment %s on %s/%s#%d: %w", s.Marker, owner, repo, number, err)
	}

	if id != 0 {
		written, err := c.editComment(ctx, owner, repo, id, raw)
		if err == nil {
			return written, nil
		}
		if !errors.Is(err, ErrNotFound) || s.EditOnly {
			return 0, err
		}
		// Deleted between the listing and the edit. Posting a new one is the
		// same outcome the deletion path already has.
		c.log.Info("sticky comment disappeared while being edited, posting a new one",
			"repo", owner+"/"+repo, "pr", number, "marker", s.Marker, "id", id)
	}

	path, err := repoPath(owner, repo, "issues", fmt.Sprint(number), "comments")
	if err != nil {
		return 0, err
	}
	var written issueComment
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &written); err != nil {
		return 0, fmt.Errorf("post comment %s on %s/%s#%d: %w", s.Marker, owner, repo, number, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("post comment %s on %s/%s#%d: response carried no id", s.Marker, owner, repo, number)
	}
	return written.ID, nil
}

// editComment PATCHes an existing comment by id.
func (c *Client) editComment(ctx context.Context, owner, repo string, id int64, body []byte) (int64, error) {
	path, err := repoPath(owner, repo, "issues", "comments", fmt.Sprint(id))
	if err != nil {
		return 0, err
	}
	var written issueComment
	if _, err := c.do(ctx, request{method: http.MethodPatch, path: path, body: body}, &written); err != nil {
		return 0, fmt.Errorf("edit comment %d on %s/%s: %w", id, owner, repo, err)
	}
	return id, nil
}

// CreateComment posts body as a new comment on pull request number. Unlike
// UpsertComment it is never looked up or edited again — for a reply to a
// one-off ask (`/why`) rather than an ongoing verdict: a later ask at a later
// sha is a different question, not an edit to this one.
func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	if number <= 0 {
		return 0, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if strings.TrimSpace(body) == "" {
		return 0, errors.New("comment needs a body")
	}

	raw, err := json.Marshal(issueComment{Body: truncate(body)})
	if err != nil {
		return 0, fmt.Errorf("encode comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	path, err := repoPath(owner, repo, "issues", fmt.Sprint(number), "comments")
	if err != nil {
		return 0, err
	}
	var written issueComment
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &written); err != nil {
		return 0, fmt.Errorf("post comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("post comment on %s/%s#%d: response carried no id", owner, repo, number)
	}
	return written.ID, nil
}

// CommentBody returns the body of the oldest comment carrying marker, or ""
// when the topic has no comment on this pull request. It is how a topic that
// carries state — the assignment ledger — reads back what the last run wrote,
// and an absent comment is an empty state rather than an error.
func (c *Client) CommentBody(ctx context.Context, owner, repo string, number int, marker string) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if strings.TrimSpace(marker) == "" {
		return "", errors.New("cannot look a comment up without a marker")
	}
	_, body, err := c.findComment(ctx, owner, repo, number, marker)
	return body, err
}

// findComment returns the id and body of the oldest comment carrying marker, or
// 0 and "" when there is none.
func (c *Client) findComment(ctx context.Context, owner, repo string, number int, marker string) (int64, string, error) {
	path, err := repoPath(owner, repo, "issues", fmt.Sprint(number), "comments")
	if err != nil {
		return 0, "", err
	}
	comments, err := paginate[issueComment](ctx, c, path, nil)
	if err != nil {
		return 0, "", fmt.Errorf("list comments on %s/%s#%d: %w", owner, repo, number, err)
	}

	var id int64
	var body string
	var seen int
	for _, cm := range comments {
		if cm.ID == 0 || !strings.Contains(cm.Body, marker) {
			continue
		}
		seen++
		if id == 0 || cm.ID < id {
			id, body = cm.ID, cm.Body
		}
	}
	if seen > 1 {
		// An earlier run wrote a duplicate. Editing the oldest is deterministic,
		// so this run does not add a third, and the extras stay for a human to
		// delete — Talooner never deletes a comment.
		c.log.Warn("more than one comment carries the marker, editing the oldest",
			"repo", owner+"/"+repo, "pr", number, "marker", marker, "count", seen, "id", id)
	}
	return id, body, nil
}
