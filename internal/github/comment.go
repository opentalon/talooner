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

const maxCommentBytes = 65536

const truncationNotice = "\n\n… truncated: this comment hit GitHub's size limit. The check run carries the full verdict.\n"

type StickyComment struct {
	Marker   string
	Body     string
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
		return fmt.Errorf("sticky comment %s needs a body", s.Marker)
	}
	if strings.Contains(s.Body, s.Marker) {
		return fmt.Errorf("sticky comment %s carries its own marker in the body", s.Marker)
	}
	return nil
}

func (s StickyComment) text() string {
	return truncate(s.Marker + "\n" + s.Body)
}

func truncate(s string) string {
	if len(s) <= maxCommentBytes {
		return s
	}
	keep := maxCommentBytes - len(truncationNotice)
	s = s[:max(keep, 0)]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + truncationNotice
}

type issueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

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
		c.log.Warn("more than one comment carries the marker, editing the oldest",
			"repo", owner+"/"+repo, "pr", number, "marker", marker, "count", seen, "id", id)
	}
	return id, body, nil
}
