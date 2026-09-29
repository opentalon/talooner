package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const maxLastToucherPaths = 25

type lastToucherCommit struct {
	Commit struct {
		Author struct {
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

func (c *Client) LastToucher(ctx context.Context, owner, repo, baseSHA string, paths []string) (string, error) {
	if len(paths) > maxLastToucherPaths {
		paths = paths[:maxLastToucherPaths]
	}

	path, err := repoPath(owner, repo, "commits")
	if err != nil {
		return "", err
	}

	var bestLogin string
	var bestDate time.Time
	for _, p := range paths {
		query := url.Values{"path": {p}, "sha": {baseSHA}, "per_page": {"1"}}
		var commits []lastToucherCommit
		if _, err := c.do(ctx, request{method: http.MethodGet, path: path, query: query}, &commits); err != nil {
			return "", fmt.Errorf("last commit touching %s in %s/%s: %w", p, owner, repo, err)
		}
		if len(commits) == 0 || commits[0].Author == nil || commits[0].Author.Login == "" {
			continue
		}
		if date := commits[0].Commit.Author.Date; date.After(bestDate) {
			bestDate = date
			bestLogin = commits[0].Author.Login
		}
	}
	return bestLogin, nil
}
