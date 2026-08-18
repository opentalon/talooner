package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// maxFileBytes caps a file read through the Contents API. The endpoint itself
// refuses to inline anything above 1 MiB, so this is the same limit stated on
// our side rather than a policy of its own.
const maxFileBytes = 1 << 20

// FileContent reads one file from a repo at ref, which is a branch, tag or sha.
//
// Every ruleset read passes ref explicitly: the ruleset that governs a run comes
// from the base branch, and a call that defaulted to the repo's HEAD would
// quietly read an attacker-editable file on a fork PR (architecture.md, "Fork
// safety").
//
// A missing file comes back as ErrNotFound, which for the ruleset is an answer —
// the repo has not onboarded — rather than a failure.
func (c *Client) FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("file path is required for %s/%s", owner, repo)
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("ref is required to read %s from %s/%s", path, owner, repo)
	}
	// The file path is many segments, and repoPath escapes what it is given as
	// one, so it is split first. A ".." segment is refused rather than escaped:
	// it would resolve server-side and read a file outside the directory the
	// caller asked about.
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, s := range segments {
		if s == ".." || s == "." {
			return nil, fmt.Errorf("file path %q may not contain %q", path, s)
		}
	}
	apiPath, err := repoPath(owner, repo, append([]string{"contents"}, segments...)...)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Type     string `json:"type"`
		Size     int    `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	req := request{method: http.MethodGet, path: apiPath, query: url.Values{"ref": {ref}}}
	if _, err := c.do(ctx, req, &payload); err != nil {
		return nil, fmt.Errorf("read %s from %s/%s@%s: %w", path, owner, repo, ref, err)
	}

	if payload.Type != "file" {
		return nil, fmt.Errorf("read %s from %s/%s@%s: entry is a %s, not a file", path, owner, repo, ref, payload.Type)
	}
	if payload.Size > maxFileBytes {
		return nil, fmt.Errorf("read %s from %s/%s@%s: %d bytes is over the %d byte limit",
			path, owner, repo, ref, payload.Size, maxFileBytes)
	}
	// GitHub drops the inline content for a large file and reports encoding
	// "none". Treating that as an empty file would read as an empty ruleset,
	// which is a valid ruleset that approves nothing — a wrong answer, not a
	// failed one.
	if payload.Encoding != "base64" {
		return nil, fmt.Errorf("read %s from %s/%s@%s: unexpected encoding %q", path, owner, repo, ref, payload.Encoding)
	}
	// The API wraps the base64 at 60 columns.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode %s from %s/%s@%s: %w", path, owner, repo, ref, err)
	}
	return raw, nil
}
