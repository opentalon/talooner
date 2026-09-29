package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/opentalon/talooner/internal/host"
)

const mergeablePollAttempts = 5

func projectPath(owner, repo string, rest ...string) (string, error) {
	if owner == "" || repo == "" {
		return "", fmt.Errorf("owner and repo are required, got %q/%q", owner, repo)
	}
	id := url.PathEscape(owner + "/" + repo)
	segments := make([]string, 0, len(rest)+1)
	segments = append(segments, id)
	for _, s := range rest {
		if s == "" {
			return "", fmt.Errorf("empty path segment in /projects/%s/%s", id, strings.Join(rest, "/"))
		}
		segments = append(segments, url.PathEscape(s))
	}
	return "/projects/" + strings.Join(segments, "/"), nil
}

type mrPayload struct {
	IID                 int    `json:"iid"`
	SourceProjectID     int    `json:"source_project_id"`
	TargetProjectID     int    `json:"target_project_id"`
	Title               string `json:"title"`
	Description         string `json:"description"`
	State               string `json:"state"`
	SourceBranch        string `json:"source_branch"`
	TargetBranch        string `json:"target_branch"`
	Draft               bool   `json:"draft"`
	WorkInProgress      bool   `json:"work_in_progress"`
	MergeStatus         string `json:"merge_status"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	SHA                 string `json:"sha"`
	Author              *struct {
		Username string `json:"username"`
	} `json:"author"`
	DiffRefs *struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
	Labels    []string `json:"labels"`
	Assignees []struct {
		Username string `json:"username"`
	} `json:"assignees"`
	Reviewers []struct {
		Username string `json:"username"`
	} `json:"reviewers"`
}

func (c *Client) PullRequest(ctx context.Context, owner, repo string, number int) (*host.PullRequest, error) {
	if number <= 0 {
		return nil, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number))
	if err != nil {
		return nil, err
	}

	var p mrPayload
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &p); err != nil {
		return nil, fmt.Errorf("fetch merge request %s/%s!%d: %w", owner, repo, number, err)
	}
	if p.SHA == "" && (p.DiffRefs == nil || p.DiffRefs.HeadSHA == "") {
		return nil, fmt.Errorf("merge request %s/%s!%d came back with no head sha", owner, repo, number)
	}

	headSHA, baseSHA := p.SHA, ""
	if p.DiffRefs != nil {
		headSHA, baseSHA = p.DiffRefs.HeadSHA, p.DiffRefs.BaseSHA
	}

	pr := &host.PullRequest{
		Number:    p.IID,
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		HeadRef:   p.SourceBranch,
		BaseRef:   p.TargetBranch,
		Title:     p.Title,
		Body:      p.Description,
		State:     mrState(p.State),
		Draft:     p.Draft || p.WorkInProgress,
		Merged:    p.State == "merged",
		Mergeable: mrMergeable(p.DetailedMergeStatus, p.MergeStatus),
		Labels:    p.Labels,
		IsFork:    p.SourceProjectID != 0 && p.TargetProjectID != 0 && p.SourceProjectID != p.TargetProjectID,
	}
	if pr.Number == 0 {
		pr.Number = number
	}
	if p.Author != nil {
		pr.Author = p.Author.Username
	}
	for _, a := range p.Assignees {
		pr.Assignees = append(pr.Assignees, a.Username)
	}
	for _, r := range p.Reviewers {
		pr.Requested.Users = append(pr.Requested.Users, r.Username)
	}
	return pr, nil
}

func mrState(s string) string {
	if s == "opened" {
		return "open"
	}
	return "closed"
}

func mrMergeable(detailed, legacy string) *bool {
	pending := map[string]bool{"checking": true, "unchecked": true, "preparing": true, "ci_still_running": true}
	t, f := true, false
	if detailed != "" {
		if detailed == "mergeable" {
			return &t
		}
		if pending[detailed] {
			return nil
		}
		return &f
	}
	switch legacy {
	case "can_be_merged":
		return &t
	case "cannot_be_merged":
		return &f
	default:
		return nil
	}
}

func (c *Client) ResolveMergeable(ctx context.Context, owner, repo string, number int) (*host.PullRequest, error) {
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
			return nil, fmt.Errorf("wait to resolve mergeability of %s/%s!%d: %w", owner, repo, number, err)
		}
	}
}

type diffEntry struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
}

func (c *Client) ChangedFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	if number <= 0 {
		return nil, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "diffs")
	if err != nil {
		return nil, err
	}

	entries, err := paginate[diffEntry](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list changed files of %s/%s!%d: %w", owner, repo, number, err)
	}

	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if p := changedPath(e); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func (c *Client) ChangedFileStats(ctx context.Context, owner, repo string, number int) ([]host.FileStat, error) {
	if number <= 0 {
		return nil, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number), "diffs")
	if err != nil {
		return nil, err
	}

	entries, err := paginate[diffEntry](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("list changed files of %s/%s!%d: %w", owner, repo, number, err)
	}

	stats := make([]host.FileStat, 0, len(entries))
	for _, e := range entries {
		p := changedPath(e)
		if p == "" {
			continue
		}
		add, del := diffStat(e.Diff)
		stats = append(stats, host.FileStat{Path: p, Additions: add, Deletions: del})
	}
	return stats, nil
}

func changedPath(e diffEntry) string {
	if e.NewPath != "" {
		return e.NewPath
	}
	return e.OldPath
}

func diffStat(diff string) (additions, deletions int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

var writeAccess = map[int]bool{30: true, 40: true, 50: true}

func (c *Client) HasWriteAccess(ctx context.Context, owner, repo, login string) (bool, error) {
	if login == "" {
		return false, errors.New("cannot check write access for an empty login")
	}

	var users []struct {
		ID int `json:"id"`
	}
	if _, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/users",
		query:  url.Values{"username": {login}},
	}, &users); err != nil {
		return false, fmt.Errorf("look up gitlab user %s: %w", login, err)
	}
	if len(users) == 0 {
		return false, nil
	}

	path, err := projectPath(owner, repo, "members", "all", strconv.Itoa(users[0].ID))
	if err != nil {
		return false, err
	}
	var member struct {
		AccessLevel int `json:"access_level"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &member); err != nil {
		if errors.Is(err, host.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("check membership for %s on %s/%s: %w", login, owner, repo, err)
	}
	return writeAccess[member.AccessLevel], nil
}
