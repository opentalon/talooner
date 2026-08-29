package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// maxLastToucherPaths bounds how many distinct changed paths LastToucher queries
// commit history for. The commits API takes one path per call, so this is a real
// cost/latency cap, not cosmetic: a PR touching more files than this still
// resolves over just the first maxLastToucherPaths (changed-file order), a
// documented, deterministic scope rather than a guess (facts.md,
// "user.last_toucher").
const maxLastToucherPaths = 25

// lastToucherCommit is the part of a commits-list entry LastToucher reads: the
// git commit's author date, which decides "most recent", and the linked GitHub
// account, which is nullable when the commit's author has none.
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

// LastToucher finds the author of the most recent commit that touched any of
// paths, walking history from baseSHA — the same trust boundary as CODEOWNERS
// and the ruleset (architecture.md, "Fork safety"), so a fork PR's own commits
// are never in view. It is the second tier of user.owner resolution (facts.md,
// "user.owner"), meant to be called only when CODEOWNERS names nobody.
//
// paths is queried one at a time — GitHub's commits endpoint takes a single
// path per call — capped at maxLastToucherPaths. The winner across every query
// is the single most recent commit by author date; its GitHub login is
// returned. A commit whose author has no linked GitHub account, or a path with
// no prior commit at all (a file this PR adds), contributes nothing rather than
// a guess from the raw git name or email. LastToucher returns "" — not an
// error — when nothing in the queried paths resolves to a login.
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
