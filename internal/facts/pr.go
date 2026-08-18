package facts

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner/internal/github"
)

// Source is the part of *github.Client that pr.* extraction needs.
type Source interface {
	PullRequest(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	ChangedFiles(ctx context.Context, owner, repo string, number int) ([]string, error)
}

// PR extracts the built-in pr.* facts (facts.md, "Built-in pr.* facts"). They
// are a pure function of the PR at its head sha, so a run re-extracts rather
// than caching.
//
// Two API calls, both required: the PR itself and its full file list. Either
// failing fails the whole extraction — see the package comment for why a
// partial set is the dangerous outcome.
func PR(ctx context.Context, src Source, owner, repo string, number int) (Set, error) {
	pr, err := src.PullRequest(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("extract pr.* facts for %s/%s#%d: %w", owner, repo, number, err)
	}
	if pr == nil {
		return nil, fmt.Errorf("extract pr.* facts for %s/%s#%d: no pull request returned", owner, repo, number)
	}

	changed, err := src.ChangedFiles(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("extract pr.changed_files for %s/%s#%d: %w", owner, repo, number, err)
	}

	s := New()
	s.Int("pr.number", pr.Number)
	s.String("pr.head_sha", pr.HeadSHA)
	s.String("pr.base_sha", pr.BaseSHA)
	s.String("pr.author", pr.Author)
	s.Bool("pr.is_fork", pr.IsFork)
	s.Bool("pr.draft", pr.Draft)
	s.String("pr.title", pr.Title)
	s.String("pr.body", pr.Body)
	s.Bool("pr.has_description", strings.TrimSpace(pr.Body) != "")
	s.Int("pr.additions", pr.Additions)
	s.Int("pr.deletions", pr.Deletions)
	s.Int("pr.lines_changed", pr.Additions+pr.Deletions)
	s.Int("pr.files_changed", pr.ChangedFiles)
	s.Int("pr.commits", pr.Commits)
	s.Strings("pr.changed_files", changed)
	s.Strings("pr.labels", pr.Labels)
	return s, nil
}
