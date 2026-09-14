package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type CheckRunReport struct {
	Name       string
	Status     string
	Conclusion string
}

type CommitStatus struct {
	Context string
	State   string
}

type Checks struct {
	Runs     []CheckRunReport
	Statuses []CommitStatus
}

func (c Checks) Pending() bool {
	for _, r := range c.Runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			return true
		}
	}
	for _, s := range c.Statuses {
		if s.State == "pending" {
			return true
		}
	}
	return false
}

func (c *Client) CommitChecks(ctx context.Context, owner, repo, sha string) (Checks, error) {
	if sha == "" {
		return Checks{}, fmt.Errorf("head sha is required to fetch checks")
	}
	path, err := repoPath(owner, repo, "commits", sha, "check-runs")
	if err != nil {
		return Checks{}, err
	}

	var checks Checks
	if err := c.collectObject(ctx, path, nil, func(raw json.RawMessage) error {
		var page struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode check runs at %s: %w", path, err)
		}
		for _, r := range page.CheckRuns {
			checks.Runs = append(checks.Runs, CheckRunReport{
				Name:       r.Name,
				Status:     r.Status,
				Conclusion: r.Conclusion,
			})
		}
		return nil
	}); err != nil {
		return Checks{}, err
	}

	path, err = repoPath(owner, repo, "commits", sha, "status")
	if err != nil {
		return Checks{}, err
	}
	if err := c.collectObject(ctx, path, nil, func(raw json.RawMessage) error {
		var page struct {
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode commit statuses at %s: %w", path, err)
		}
		for _, s := range page.Statuses {
			checks.Statuses = append(checks.Statuses, CommitStatus{
				Context: s.Context,
				State:   s.State,
			})
		}
		return nil
	}); err != nil {
		return Checks{}, err
	}
	return checks, nil
}

func (c *Client) collectObject(ctx context.Context, path string, query url.Values, each func(json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", strconv.Itoa(perPage))

	req := request{method: http.MethodGet, path: path, query: query}
	for page := 1; ; page++ {
		if page > maxPages {
			return fmt.Errorf("%s returned more than %d pages, refusing to keep paging", path, maxPages)
		}
		var raw json.RawMessage
		header, err := c.do(ctx, req, &raw)
		if err != nil {
			return fmt.Errorf("page %d of %s: %w", page, path, err)
		}
		if err := each(raw); err != nil {
			return err
		}
		next := nextLink(header.Get("Link"))
		if next == "" {
			return nil
		}
		req = request{method: http.MethodGet, path: next}
	}
}
