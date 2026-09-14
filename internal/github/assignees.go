package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

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

func (c *Client) AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	return c.writeAssignees(ctx, http.MethodPost, owner, repo, number, logins)
}

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

func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, users, teams []string) (Reviewers, error) {
	return c.writeReviewRequests(ctx, http.MethodPost, owner, repo, number, users, teams)
}

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

func verbOf(method string) string {
	if method == http.MethodDelete {
		return "remove"
	}
	return "add"
}
