package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// PullRequest is the part of a PR that facts are built from (facts.md,
// "pr.*"). Everything here comes from one GET, which is also how an
// issue_comment run learns its head sha — that payload carries none.
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
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changed_files"`
	Commits      int    `json:"commits"`
	Labels       []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// PullRequest fetches one PR.
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
		Additions:    p.Additions,
		Deletions:    p.Deletions,
		ChangedFiles: p.ChangedFiles,
		Commits:      p.Commits,
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
	// A deleted head repo reads as nil, which is not the base repo, so it counts
	// as a fork — the cautious way round for a fact that gates secrets.
	pr.IsFork = p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName
	return pr, nil
}

// ChangedFiles lists every path the PR touches, following pagination to the end.
// A PR bigger than the page cap fails rather than returning a prefix: rules
// asserting on pr.changed_files would silently miss the rest (facts.md).
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

// writeAccess is the set of permission levels that may run a command. GitHub
// reports "maintain" as its own level rather than as "write".
var writeAccess = map[string]bool{"admin": true, "maintain": true, "write": true}

// HasWriteAccess reports whether login may run Talooner commands on owner/repo.
// It implements command.PermissionChecker.
//
// A login GitHub does not know as a collaborator comes back 404, which is an
// answer — false, no error. Every other failure is an error: a permission API
// that 500s must fail the run rather than quietly reading as "not authorised"
// and dropping a maintainer's command.
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

// repoPath builds /repos/{owner}/{repo}/... with every segment escaped, so a
// login or branch name carrying a slash cannot reach another endpoint.
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
