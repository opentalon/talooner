package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const maxLastToucherPaths = 25

type toucherCommit struct {
	AuthorEmail string    `json:"author_email"`
	AuthoredAt  time.Time `json:"authored_date"`
}

// LastToucher correlates git commit authorship to a GitLab account by email,
// the same "no linked account, no answer" rule the GitHub backend applies:
// GitLab's commits API carries only the raw git author name/email, never a
// platform login, so an email that doesn't resolve to a GitLab user
// contributes nothing rather than falling back to the raw git identity.
func (c *Client) LastToucher(ctx context.Context, owner, repo, baseSHA string, paths []string) (string, error) {
	if len(paths) > maxLastToucherPaths {
		paths = paths[:maxLastToucherPaths]
	}

	path, err := projectPath(owner, repo, "repository", "commits")
	if err != nil {
		return "", err
	}

	var bestLogin string
	var bestDate time.Time
	for _, p := range paths {
		query := url.Values{"path": {p}, "ref_name": {baseSHA}, "per_page": {"1"}}
		var commits []toucherCommit
		if _, err := c.do(ctx, request{method: http.MethodGet, path: path, query: query}, &commits); err != nil {
			return "", fmt.Errorf("last commit touching %s in %s/%s: %w", p, owner, repo, err)
		}
		if len(commits) == 0 || commits[0].AuthorEmail == "" {
			continue
		}
		if date := commits[0].AuthoredAt; !date.After(bestDate) {
			continue
		}
		login, err := c.userByEmail(ctx, commits[0].AuthorEmail)
		if err != nil {
			return "", err
		}
		if login == "" {
			continue
		}
		bestDate, bestLogin = commits[0].AuthoredAt, login
	}
	return bestLogin, nil
}

func (c *Client) userByEmail(ctx context.Context, email string) (string, error) {
	var users []struct {
		Username string `json:"username"`
	}
	if _, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/users",
		query:  url.Values{"search": {email}},
	}, &users); err != nil {
		return "", fmt.Errorf("look up gitlab user for %s: %w", email, err)
	}
	if len(users) == 0 {
		return "", nil
	}
	return users[0].Username, nil
}
