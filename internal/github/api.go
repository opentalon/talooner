package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const mergeablePollAttempts = 5

type PullRequest struct {
	Number       int
	HeadSHA      string
	BaseSHA      string
	HeadRef      string
	BaseRef      string
	Author       string
	Title        string
	Body         string
	State        string
	Draft        bool
	Merged       bool
	IsFork       bool
	Additions    int
	Deletions    int
	ChangedFiles int
	Commits      int
	Labels       []string
	Mergeable    *bool
	Assignees    []string
	Requested    Reviewers
}

type FileStat struct {
	Path      string
	Additions int
	Deletions int
}

type pullRequestPayload struct {
	Number int `json:"number"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	Merged       bool   `json:"merged"`
	Mergeable    *bool  `json:"mergeable"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changed_files"`
	Commits      int    `json:"commits"`
	Labels       []struct {
		Name string `json:"name"`
	} `json:"labels"`
	assigneesPayload
	reviewersPayload
}

func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	if number <= 0 {
		return nil, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number))
	if err != nil {
		return nil, err
	}

	var p pullRequestPayload
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &p); err != nil {
		return nil, fmt.Errorf("fetch pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	if p.Head.SHA == "" {
		return nil, fmt.Errorf("pull request %s/%s#%d came back with no head sha", owner, repo, number)
	}

	pr := &PullRequest{
		Number:       p.Number,
		HeadSHA:      p.Head.SHA,
		BaseSHA:      p.Base.SHA,
		HeadRef:      p.Head.Ref,
		BaseRef:      p.Base.Ref,
		Title:        p.Title,
		Body:         p.Body,
		State:        p.State,
		Draft:        p.Draft,
		Merged:       p.Merged,
		Mergeable:    p.Mergeable,
		Additions:    p.Additions,
		Deletions:    p.Deletions,
		ChangedFiles: p.ChangedFiles,
		Commits:      p.Commits,
		Assignees:    p.logins(),
		Requested:    p.reviewers(),
	}
	if pr.Number == 0 {
		pr.Number = number
	}
	if p.User != nil {
		pr.Author = p.User.Login
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	pr.IsFork = p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName
	return pr, nil
}

func (c *Client) ResolveMergeable(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	for attempt := 0; ; attempt++ {
		pr, err := c.PullRequest(ctx, owner, repo, number)
		if err != nil {
			return nil, err
		}
		if pr.Mergeable != nil || pr.State != "open" || pr.Merged {
			return pr, nil
		}
		if attempt >= mergeablePollAttempts {
			return pr, nil
		}
		if err := c.sleep(ctx, c.backoff(attempt)); err != nil {
			return nil, fmt.Errorf("wait to resolve mergeability of %s/%s#%d: %w", owner, repo, number, err)
		}
	}
}

func (c *Client) ChangedFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if number <= 0 {
		return nil, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "files")
	if err != nil {
		return nil, err
	}

	type file struct {
		Filename string `json:"filename"`
	}
	files, err := paginate[file](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list changed files of %s/%s#%d: %w", owner, repo, number, err)
	}

	paths := make([]string, 0, len(files))
	for _, f := range files {
		if f.Filename != "" {
			paths = append(paths, f.Filename)
		}
	}
	return paths, nil
}

type fileStat struct {
	Filename  string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

func (c *Client) ChangedFileStats(ctx context.Context, owner, repo string, number int) ([]FileStat, error) {
	if number <= 0 {
		return nil, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "files")
	if err != nil {
		return nil, err
	}

	files, err := paginate[fileStat](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list changed files of %s/%s#%d: %w", owner, repo, number, err)
	}

	stats := make([]FileStat, 0, len(files))
	for _, f := range files {
		if f.Filename == "" {
			continue
		}
		stats = append(stats, FileStat{Path: f.Filename, Additions: f.Additions, Deletions: f.Deletions})
	}
	return stats, nil
}

var writeAccess = map[string]bool{"admin": true, "maintain": true, "write": true}

func (c *Client) HasWriteAccess(ctx context.Context, owner, repo, login string) (bool, error) {
	if login == "" {
		return false, errors.New("cannot check write access for an empty login")
	}
	path, err := repoPath(owner, repo, "collaborators", login, "permission")
	if err != nil {
		return false, err
	}

	var payload struct {
		Permission string `json:"permission"`
		User       *struct {
			RoleName string `json:"role_name"`
		} `json:"user"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &payload); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("check write access for %s on %s/%s: %w", login, owner, repo, err)
	}

	if writeAccess[strings.ToLower(payload.Permission)] {
		return true, nil
	}
	if payload.User != nil && writeAccess[strings.ToLower(payload.User.RoleName)] {
		return true, nil
	}
	return false, nil
}

func repoPath(owner, repo string, rest ...string) (string, error) {
	if owner == "" || repo == "" {
		return "", fmt.Errorf("owner and repo are required, got %q/%q", owner, repo)
	}
	segments := append([]string{"repos", owner, repo}, rest...)
	escaped := make([]string, len(segments))
	for i, s := range segments {
		if s == "" {
			return "", fmt.Errorf("empty path segment in /%s", strings.Join(segments, "/"))
		}
		escaped[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(escaped, "/"), nil
}
