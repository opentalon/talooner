package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const maxFileBytes = 1 << 20

func (c *Client) FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("file path is required for %s/%s", owner, repo)
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("ref is required to read %s from %s/%s", path, owner, repo)
	}
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
	if payload.Encoding != "base64" {
		return nil, fmt.Errorf("read %s from %s/%s@%s: unexpected encoding %q", path, owner, repo, ref, payload.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode %s from %s/%s@%s: %w", path, owner, repo, ref, err)
	}
	return raw, nil
}
