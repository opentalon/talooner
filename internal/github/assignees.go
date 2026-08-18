package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Reviewers is the set of review requests standing on a pull request: users by
// login, teams by slug. GitHub drops a request from this list as soon as the
// review is submitted, so a request a human already fulfilled is simply absent —
// which is what makes "leave the completed review alone" the default rather than
// a special case anything has to check.
type Reviewers struct {
	Users []string
	Teams []string
}

type reviewersPayload struct {
	Users []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
	Teams []struct {
		Slug string `json:"slug"`
	} `json:"requested_teams"`
}

func (p reviewersPayload) reviewers() Reviewers {
	var rv Reviewers
	for _, u := range p.Users {
		if u.Login != "" {
			rv.Users = append(rv.Users, u.Login)
		}
	}
	for _, t := range p.Teams {
		if t.Slug != "" {
			rv.Teams = append(rv.Teams, t.Slug)
		}
	}
	return rv
}

type assigneesPayload struct {
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
}

func (p assigneesPayload) logins() []string {
	out := make([]string, 0, len(p.Assignees))
	for _, a := range p.Assignees {
		if a.Login != "" {
			out = append(out, a.Login)
		}
	}
	return out
}

// AddAssignees assigns logins on the pull request and returns the assignees
// GitHub reports afterwards — every one of them, not only the ones added.
//
// The return value is the point. GitHub accepts an assignee the login cannot
// actually be given (no repo access, or the eleventh assignee on a PR that
// already has ten) with a 201 and silently leaves it out, so the response is the
// only place a caller can learn that a write it was told succeeded did nothing.
func (c *Client) AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	return c.writeAssignees(ctx, http.MethodPost, owner, repo, number, logins)
}

// RemoveAssignees unassigns logins and returns the assignees left standing. A
// login that is not assigned is not an error: GitHub ignores it, and the outcome
// is the one the call wanted.
func (c *Client) RemoveAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	return c.writeAssignees(ctx, http.MethodDelete, owner, repo, number, logins)
}

func (c *Client) writeAssignees(ctx context.Context, method, owner, repo string, number int, logins []string) ([]string, error) {
	if number <= 0 {
		return nil, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if len(logins) == 0 {
		return nil, fmt.Errorf("no assignees to %s on %s/%s#%d", verbOf(method), owner, repo, number)
	}
	path, err := repoPath(owner, repo, "issues", fmt.Sprint(number), "assignees")
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(struct {
		Assignees []string `json:"assignees"`
	}{Assignees: logins})
	if err != nil {
		return nil, fmt.Errorf("encode assignees for %s/%s#%d: %w", owner, repo, number, err)
	}

	var payload assigneesPayload
	if _, err := c.do(ctx, request{method: method, path: path, body: raw}, &payload); err != nil {
		return nil, fmt.Errorf("%s assignees %v on %s/%s#%d: %w", verbOf(method), logins, owner, repo, number, err)
	}
	return payload.logins(), nil
}

// RequestReviewers asks users and teams for a review and returns the requests
// standing afterwards.
//
// GitHub rejects the whole call with a 422 when any one of them cannot be asked
// — the PR's own author, a login with no access, a team slug that does not exist
// — so unlike assignees there is no silent half-success here, and the error is
// the report.
func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, users, teams []string) (Reviewers, error) {
	return c.writeReviewRequests(ctx, http.MethodPost, owner, repo, number, users, teams)
}

// RemoveReviewRequests withdraws the requests for users and teams. A request
// that is not standing — withdrawn already, or fulfilled by a review — is not an
// error.
func (c *Client) RemoveReviewRequests(ctx context.Context, owner, repo string, number int, users, teams []string) (Reviewers, error) {
	return c.writeReviewRequests(ctx, http.MethodDelete, owner, repo, number, users, teams)
}

func (c *Client) writeReviewRequests(ctx context.Context, method, owner, repo string, number int, users, teams []string) (Reviewers, error) {
	if number <= 0 {
		return Reviewers{}, fmt.Errorf("pull request number must be positive, got %d", number)
	}
	if len(users) == 0 && len(teams) == 0 {
		return Reviewers{}, fmt.Errorf("no reviewers to %s on %s/%s#%d", verbOf(method), owner, repo, number)
	}
	path, err := repoPath(owner, repo, "pulls", fmt.Sprint(number), "requested_reviewers")
	if err != nil {
		return Reviewers{}, err
	}
	raw, err := json.Marshal(struct {
		Reviewers     []string `json:"reviewers"`
		TeamReviewers []string `json:"team_reviewers"`
	}{Reviewers: users, TeamReviewers: teams})
	if err != nil {
		return Reviewers{}, fmt.Errorf("encode review requests for %s/%s#%d: %w", owner, repo, number, err)
	}

	var payload reviewersPayload
	if _, err := c.do(ctx, request{method: method, path: path, body: raw}, &payload); err != nil {
		return Reviewers{}, fmt.Errorf("%s review requests (users %v, teams %v) on %s/%s#%d: %w",
			verbOf(method), users, teams, owner, repo, number, err)
	}
	return payload.reviewers(), nil
}

// verbOf is the word an error message uses for the write that failed. The HTTP
// method is the wrong thing to show a maintainer reading a log line.
func verbOf(method string) string {
	if method == http.MethodDelete {
		return "remove"
	}
	return "add"
}
