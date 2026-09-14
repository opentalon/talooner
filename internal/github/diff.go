package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const DiffMaxBytes = 1 << 20

type filePatch struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

func (c *Client) Diff(ctx context.Context, owner, repo string, number, maxBytes int) (string, bool, error) {
	if number <= 0 {
		return "", false, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if maxBytes <= 0 {
		return "", false, fmt.Errorf("diff cap must be positive, got %d", maxBytes)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "files")
	if err != nil {
		return "", false, err
	}

	query := url.Values{}
	query.Set("per_page", strconv.Itoa(perPage))
	req := request{method: http.MethodGet, path: path, query: query}

	var buf strings.Builder
	var truncated bool
	for page := 1; ; page++ {
		if page > maxPages {
			return "", false, fmt.Errorf("%s returned more than %d pages, refusing to keep paging", path, maxPages)
		}
		var files []filePatch
		header, err := c.do(ctx, req, &files)
		if err != nil {
			return "", false, fmt.Errorf("page %d of %s: %w", page, path, err)
		}
		for _, f := range files {
			if f.Patch == "" {
				continue
			}
			add := f.Patch
			if buf.Len() > 0 {
				add = "\n" + f.Patch
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
