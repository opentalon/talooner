package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// DiffMaxBytes is the cap on pr.diff. A diff bigger than this is truncated and
// the pr.diff_truncated fact is set so a rule — and v1.5's llm_review — can tell
// a complete diff from a capped one. 1 MiB is the v1 default; a tenant cap from
// config.yaml (E1) is passed straight through, which is why the call takes the
// limit rather than reading a global.
const DiffMaxBytes = 1 << 20

// filePatch is the part of a Files API entry that pr.diff reads. Patch is the
// unified diff; it is null for binary or oversized files, which contribute
// nothing textual to the diff.
type filePatch struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

// Diff concatenates the unified diffs of every file the PR touches, from the
// Files API, capped at maxBytes. It returns the diff and whether it was capped.
//
// The cap is file-granular: whole files are appended while they fit and the next
// one would push past the cap, at which point the loop stops and truncated is
// true. A patch that alone exceeds the cap yields an empty diff with
// truncated — nothing is silently shipped as complete, and the flag is the only
// signal a consumer gets.
//
// Binary files (null patch) are skipped. The endpoint is paginated and followed
// to the end — or until the cap — because a diff that stopped at a page boundary
// would read as a complete, smaller change.
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
				continue // binary or non-diffable file: nothing textual to add
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
