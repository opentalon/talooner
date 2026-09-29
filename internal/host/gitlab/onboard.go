package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/opentalon/talooner/internal/host"
)

func groupPath(group string, rest ...string) (string, error) {
	if group == "" {
		return "", errors.New("group is required")
	}
	id := url.PathEscape(group)
	segments := make([]string, 0, len(rest)+1)
	segments = append(segments, id)
	for _, s := range rest {
		if s == "" {
			return "", fmt.Errorf("empty path segment in /groups/%s/%s", id, strings.Join(rest, "/"))
		}
		segments = append(segments, url.PathEscape(s))
	}
	return "/groups/" + strings.Join(segments, "/"), nil
}

type ciVariable struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Masked    bool   `json:"masked"`
	Protected bool   `json:"protected"`
}

func (c *Client) UpsertProjectVariable(ctx context.Context, owner, repo, key, value string, masked bool) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("variable key is empty")
	}
	base, err := projectPath(owner, repo, "variables")
	if err != nil {
		return err
	}
	keyed, err := projectPath(owner, repo, "variables", key)
	if err != nil {
		return err
	}
	if err := c.upsertVariable(ctx, base, keyed, key, value, masked); err != nil {
		return fmt.Errorf("set variable %s on %s/%s: %w", key, owner, repo, err)
	}
	return nil
}

func (c *Client) UpsertGroupVariable(ctx context.Context, group, key, value string, masked bool) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("variable key is empty")
	}
	base, err := groupPath(group, "variables")
	if err != nil {
		return err
	}
	keyed, err := groupPath(group, "variables", key)
	if err != nil {
		return err
	}
	if err := c.upsertVariable(ctx, base, keyed, key, value, masked); err != nil {
		return fmt.Errorf("set variable %s on group %s: %w", key, group, err)
	}
	return nil
}

// upsertVariable tries PUT (update) first and falls back to POST (create) on
// a 404 — GitLab's variables API has no single upsert endpoint, only
// separate create/update ones that 400 on the wrong one.
func (c *Client) upsertVariable(ctx context.Context, listPath, itemPath, key, value string, masked bool) error {
	raw, err := json.Marshal(ciVariable{Key: key, Value: value, Masked: masked, Protected: false})
	if err != nil {
		return fmt.Errorf("encode variable %s: %w", key, err)
	}
	_, err = c.do(ctx, request{method: http.MethodPut, path: itemPath, body: raw}, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, host.ErrNotFound) {
		return err
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: listPath, body: raw}, nil); err != nil {
		return err
	}
	return nil
}

type mergeRequest struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description,omitempty"`
	WebURL       string `json:"web_url"`
}

func (c *Client) CreateMergeRequest(ctx context.Context, owner, repo, sourceBranch, targetBranch, title, description string) (string, error) {
	if strings.TrimSpace(sourceBranch) == "" || strings.TrimSpace(targetBranch) == "" {
		return "", fmt.Errorf("source and target branch are required, got %q/%q", sourceBranch, targetBranch)
	}
	if strings.TrimSpace(title) == "" {
		return "", errors.New("merge request title is empty")
	}
	path, err := projectPath(owner, repo, "merge_requests")
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(mergeRequest{
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Title:        title,
		Description:  description,
	})
	if err != nil {
		return "", fmt.Errorf("encode merge request %s -> %s on %s/%s: %w", sourceBranch, targetBranch, owner, repo, err)
	}
	var created mergeRequest
	if _, err := c.do(ctx, request{method: http.MethodPost, path: path, body: raw}, &created); err != nil {
		return "", fmt.Errorf("open merge request %s -> %s on %s/%s: %w", sourceBranch, targetBranch, owner, repo, err)
	}
	if created.WebURL == "" {
		return "", fmt.Errorf("open merge request %s -> %s on %s/%s: response carried no web_url", sourceBranch, targetBranch, owner, repo)
	}
	return created.WebURL, nil
}
