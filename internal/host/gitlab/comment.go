package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/opentalon/talooner/internal/host"
)

const maxCommentBytes = 1 << 20

const truncationNotice = "\n\n… truncated: this comment hit GitLab's note size limit. The check run carries the full verdict.\n"

func validateStickyComment(s host.StickyComment) error {
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

func stickyCommentText(s host.StickyComment) string {
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

type note struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	System bool   `json:"system"`
	Author *struct {
		Username string `json:"username"`
	} `json:"author"`
}

func (c *Client) Note(ctx context.Context, owner, repo string, number int, id int64) (string, string, error) {
	if number <= 0 {
		return "", "", fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if id <= 0 {
		return "", "", fmt.Errorf("note id must be positive, got %d", id)
	}
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "notes", strconv.FormatInt(id, 10))
	if err != nil {
		return "", "", err
	}
	var n note
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &n); err != nil {
		return "", "", fmt.Errorf("read note %d on %s/%s!%d: %w", id, owner, repo, number, err)
	}
	if n.System {
		return "", "", fmt.Errorf("read note %d on %s/%s!%d: %w: a system note is not a user comment",
			id, owner, repo, number, host.ErrNotFound)
	}
	var author string
	if n.Author != nil {
		author = n.Author.Username
	}
	return n.Body, author, nil
}

func (c *Client) UpsertComment(ctx context.Context, owner, repo string, number int, s host.StickyComment) (int64, error) {
	if number <= 0 {
		return 0, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if err := validateStickyComment(s); err != nil {
		return 0, err
	}

	id, _, err := c.findNote(ctx, owner, repo, number, s.Marker)
	if err != nil {
		return 0, err
	}
	if id == 0 && s.EditOnly {
		return 0, nil
	}

	raw, err := json.Marshal(note{Body: stickyCommentText(s)})
	if err != nil {
		return 0, fmt.Errorf("encode comment %s on %s/%s!%d: %w", s.Marker, owner, repo, number, err)
	}

	if id != 0 {
		written, err := c.editNote(ctx, owner, repo, number, id, raw)
		if err == nil {
			return written, nil
		}
		if !errors.Is(err, host.ErrNotFound) || s.EditOnly {
			return 0, err
		}
		c.log.Info("sticky comment disappeared while being edited, posting a new one",
			"repo", owner+"/"+repo, "mr", number, "marker", s.Marker, "id", id)
	}

	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "notes")
	if err != nil {
		return 0, err
	}
	var written note
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &written); err != nil {
		return 0, fmt.Errorf("post comment %s on %s/%s!%d: %w", s.Marker, owner, repo, number, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("post comment %s on %s/%s!%d: response carried no id", s.Marker, owner, repo, number)
	}
	return written.ID, nil
}

func (c *Client) editNote(ctx context.Context, owner, repo string, number int, id int64, body []byte) (int64, error) {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "notes", strconv.FormatInt(id, 10))
	if err != nil {
		return 0, err
	}
	if _, err := c.do(ctx, request{method: http.MethodPut, path: path, body: body}, nil); err != nil {
		return 0, fmt.Errorf("edit comment %d on %s/%s!%d: %w", id, owner, repo, number, err)
	}
	return id, nil
}

func (c *Client) CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	if number <= 0 {
		return 0, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if strings.TrimSpace(body) == "" {
		return 0, errors.New("comment needs a body")
	}

	raw, err := json.Marshal(note{Body: truncate(body)})
	if err != nil {
		return 0, fmt.Errorf("encode comment on %s/%s!%d: %w", owner, repo, number, err)
	}
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "notes")
	if err != nil {
		return 0, err
	}
	var written note
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &written); err != nil {
		return 0, fmt.Errorf("post comment on %s/%s!%d: %w", owner, repo, number, err)
	}
	if written.ID == 0 {
		return 0, fmt.Errorf("post comment on %s/%s!%d: response carried no id", owner, repo, number)
	}
	return written.ID, nil
}

func (c *Client) CommentBody(ctx context.Context, owner, repo string, number int, marker string) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if strings.TrimSpace(marker) == "" {
		return "", errors.New("cannot look a comment up without a marker")
	}
	_, body, err := c.findNote(ctx, owner, repo, number, marker)
	return body, err
}

func (c *Client) findNote(ctx context.Context, owner, repo string, number int, marker string) (int64, string, error) {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "notes")
	if err != nil {
		return 0, "", err
	}
	notes, err := paginate[note](ctx, c, path, nil)
	if err != nil {
		return 0, "", fmt.Errorf("list notes on %s/%s!%d: %w", owner, repo, number, err)
	}

	var id int64
	var body string
	var seen int
	for _, n := range notes {
		if n.ID == 0 || n.System || !strings.Contains(n.Body, marker) {
			continue
		}
		seen++
		if id == 0 || n.ID < id {
			id, body = n.ID, n.Body
		}
	}
	if seen > 1 {
		c.log.Warn("more than one note carries the marker, editing the oldest",
			"repo", owner+"/"+repo, "mr", number, "marker", marker, "count", seen, "id", id)
	}
	return id, body, nil
}
