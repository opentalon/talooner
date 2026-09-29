package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (c *Client) Diff(ctx context.Context, owner, repo string, number, maxBytes int) (string, bool, error) {
	if number <= 0 {
		return "", false, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if maxBytes <= 0 {
		return "", false, fmt.Errorf("diff cap must be positive, got %d", maxBytes)
	}
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "diffs")
	if err != nil {
		return "", false, err
	}

	var buf strings.Builder
	var truncated bool
	req := request{method: http.MethodGet, path: path}
	for page := 1; ; page++ {
		if page > maxPages {
			return "", false, fmt.Errorf("%s returned more than %d pages, refusing to keep paging", path, maxPages)
		}
		var entries []diffEntry
		header, err := c.do(ctx, req, &entries)
		if err != nil {
			return "", false, fmt.Errorf("page %d of %s: %w", page, path, err)
		}
		for _, e := range entries {
			if e.Diff == "" {
				continue
			}
			add := e.Diff
			if buf.Len() > 0 {
				add = "\n" + e.Diff
			}
			if buf.Len()+len(add) > maxBytes {
				truncated = true
				break
			}
			buf.WriteString(add)
		}
		if truncated {
			break
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			break
		}
		req = request{method: http.MethodGet, path: next}
	}
	return buf.String(), truncated, nil
}
