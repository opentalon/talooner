package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/opentalon/talooner/internal/host"
)

var ErrNoTeamReviewers = errors.New("gitlab merge requests have no team-reviewer field, only user reviewer_ids")

type participant struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type mrParticipantsPayload struct {
	Assignees []participant `json:"assignees"`
	Reviewers []participant `json:"reviewers"`
}

func (c *Client) mrParticipants(ctx context.Context, owner, repo string, number int) (mrParticipantsPayload, error) {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number))
	if err != nil {
		return mrParticipantsPayload{}, err
	}
	var p mrParticipantsPayload
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path}, &p); err != nil {
		return mrParticipantsPayload{}, fmt.Errorf("fetch merge request %s/%s!%d: %w", owner, repo, number, err)
	}
	return p, nil
}

func (c *Client) lookupUserIDs(ctx context.Context, logins []string) (map[string]int, error) {
	ids := make(map[string]int, len(logins))
	for _, login := range logins {
		var users []participant
		if _, err := c.do(ctx, request{
			method: http.MethodGet,
			path:   "/users",
			query:  url.Values{"username": {login}},
		}, &users); err != nil {
			return nil, fmt.Errorf("look up gitlab user %s: %w", login, err)
		}
		if len(users) == 0 {
			c.log.Warn("gitlab user not found, skipping", "login", login)
			continue
		}
		ids[login] = users[0].ID
	}
	return ids, nil
}

func usernamesOf(ps []participant) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.Username != "" {
			out = append(out, p.Username)
		}
	}
	return out
}

func (c *Client) AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	return c.writeAssignees(ctx, owner, repo, number, logins, true)
}

func (c *Client) RemoveAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error) {
	return c.writeAssignees(ctx, owner, repo, number, logins, false)
}

func (c *Client) writeAssignees(ctx context.Context, owner, repo string, number int, wantLogins []string, add bool) ([]string, error) {
	if number <= 0 {
		return nil, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if len(wantLogins) == 0 {
		return nil, fmt.Errorf("no assignees to %s on %s/%s!%d", verbOf(add), owner, repo, number)
	}

	current, err := c.mrParticipants(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	resolved, err := c.lookupUserIDs(ctx, wantLogins)
	if err != nil {
		return nil, err
	}

	ids := make(map[int]bool, len(current.Assignees))
	for _, p := range current.Assignees {
		ids[p.ID] = true
	}
	for _, id := range resolved {
		if add {
			ids[id] = true
		} else {
			delete(ids, id)
		}
	}

	updated, err := c.putParticipants(ctx, owner, repo, number, "assignee_ids", idList(ids))
	if err != nil {
		return nil, fmt.Errorf("%s assignees %v on %s/%s!%d: %w", verbOf(add), wantLogins, owner, repo, number, err)
	}
	return usernamesOf(updated.Assignees), nil
}

func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, users, teams []string) (host.Reviewers, error) {
	return c.writeReviewers(ctx, owner, repo, number, users, teams, true)
}

func (c *Client) RemoveReviewRequests(ctx context.Context, owner, repo string, number int, users, teams []string) (host.Reviewers, error) {
	return c.writeReviewers(ctx, owner, repo, number, users, teams, false)
}

func (c *Client) writeReviewers(ctx context.Context, owner, repo string, number int, users, teams []string, add bool) (host.Reviewers, error) {
	if number <= 0 {
		return host.Reviewers{}, fmt.Errorf("merge request iid must be positive, got %d", number)
	}
	if len(teams) > 0 {
		return host.Reviewers{}, fmt.Errorf("%s teams %v on %s/%s!%d: %w", verbOf(add), teams, owner, repo, number, ErrNoTeamReviewers)
	}
	if len(users) == 0 {
		return host.Reviewers{}, fmt.Errorf("no reviewers to %s on %s/%s!%d", verbOf(add), owner, repo, number)
	}

	current, err := c.mrParticipants(ctx, owner, repo, number)
	if err != nil {
		return host.Reviewers{}, err
	}
	resolved, err := c.lookupUserIDs(ctx, users)
	if err != nil {
		return host.Reviewers{}, err
	}

	ids := make(map[int]bool, len(current.Reviewers))
	for _, p := range current.Reviewers {
		ids[p.ID] = true
	}
	for _, id := range resolved {
		if add {
			ids[id] = true
		} else {
			delete(ids, id)
		}
	}

	updated, err := c.putParticipants(ctx, owner, repo, number, "reviewer_ids", idList(ids))
	if err != nil {
		return host.Reviewers{}, fmt.Errorf("%s review requests %v on %s/%s!%d: %w", verbOf(add), users, owner, repo, number, err)
	}
	return host.Reviewers{Users: usernamesOf(updated.Reviewers)}, nil
}

func (c *Client) putParticipants(ctx context.Context, owner, repo string, number int, field string, ids []int) (mrParticipantsPayload, error) {
	path, err := projectPath(owner, repo, "merge_requests", strconv.Itoa(number))
	if err != nil {
		return mrParticipantsPayload{}, err
	}
	raw, err := json.Marshal(map[string][]int{field: ids})
	if err != nil {
		return mrParticipantsPayload{}, fmt.Errorf("encode %s for %s/%s!%d: %w", field, owner, repo, number, err)
	}
	var updated mrParticipantsPayload
	if _, err := c.do(ctx, request{method: http.MethodPut, path: path, body: raw}, &updated); err != nil {
		return mrParticipantsPayload{}, err
	}
	return updated, nil
}

func idList(ids map[int]bool) []int {
	out := make([]int, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func verbOf(add bool) string {
	if add {
		return "add"
	}
	return "remove"
}
