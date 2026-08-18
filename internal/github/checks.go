package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// CheckRun is one check run on a head sha, as far as a ruleset can see it.
// Status is the run's lifecycle (queued, in_progress, completed); Conclusion is
// set once the run has completed and is what pr.tests_passing-style facts read.
type CheckRunReport struct {
	Name       string
	Status     string
	Conclusion string
}

// CommitStatus is one commit status on a head sha, the older API that predates
// check runs. State is one of pending, success, failure, error.
type CommitStatus struct {
	Context string
	State   string
}

// Checks is everything a run knows about a head sha's CI. pr.checks_pending is
// derived from it now; C3's pr.tests_passing / pr.lint_passing read the same
// fetch, so whichever lands first owns the API call (facts.md, C8's note).
type Checks struct {
	Runs     []CheckRunReport
	Statuses []CommitStatus
}

// Pending reports whether anything on the head sha is still in flight: a check
// run that is queued or in_progress, or a commit status that is pending. Zero
// of both is false, asserted — an empty CI is a settled CI, not an unknown one.
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

// CommitChecks fetches the check runs and the combined commit status at a head
// sha. Both are one round of CI, and a ruleset about a moving target needs both
// — a repo can use check runs, statuses, or neither. Either endpoint failing
// fails the call: a checks set missing its statuses would read as "nothing is
// running" to a not-shaped rule.
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

// collectObject pages through an object-shaped list endpoint — one whose page
// is {items: [...]} rather than a bare array, which is what paginate expects.
// Each page's raw body is handed to each for the caller to decode its own
// shape. Same rule as paginate: a page that errors fails the whole call, so a
// truncated set is never asserted as complete.
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
