package facts

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner/internal/github"
)

// Source is the part of *github.Client that pr.* extraction needs.
type Source interface {
	// ResolveMergeable is the PR with mergeable resolved to a bounded poll:
	// GitHub computes mergeability asynchronously and returns null until it has,
	// so the client re-fetches while null rather than handing the extractor a
	// fact it must guess. Mergeable stays nil for a closed or merged PR, which
	// never resolves.
	ResolveMergeable(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	ChangedFiles(ctx context.Context, owner, repo string, number int) ([]string, error)
	CommitChecks(ctx context.Context, owner, repo, headSHA string) (github.Checks, error)
	// Diff is the concatenated file patches, capped at maxBytes. The second
	// return is whether the cap was hit, so a rule can tell a complete diff from
	// a truncated one (issue #9).
	Diff(ctx context.Context, owner, repo string, number, maxBytes int) (string, bool, error)
}

// PR extracts the built-in pr.* facts (facts.md, "Built-in pr.* facts"). They
// are a pure function of the PR at its head sha, so a run re-extracts rather
// than caching.
//
// Four API calls, all required. ResolveMergeable can poll for several seconds
// while GitHub's mergeability job is still running, so it runs alongside
// ChangedFiles rather than blocking it; CommitChecks needs the head sha
// ResolveMergeable returns, so it fires once that settles. Any fetch failing
// fails the whole extraction — see the package comment for why a partial set
// is the dangerous outcome.
func PR(ctx context.Context, src Source, owner, repo string, number int) (Set, error) {
	type prResult struct {
		pr  *github.PullRequest
		err error
	}
	prCh := make(chan prResult, 1)
	// resolveCtx is cancelled when PR returns on any path, so the poll goroutine
	// stops burning API quota once ChangedFiles or CommitChecks fails.
	resolveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		pr, err := src.ResolveMergeable(resolveCtx, owner, repo, number)
		prCh <- prResult{pr, err}
	}()

	changed, err := src.ChangedFiles(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("extract pr.changed_files for %s/%s#%d: %w", owner, repo, number, err)
	}

	res := <-prCh
	if res.err != nil {
		return nil, fmt.Errorf("extract pr.* facts for %s/%s#%d: %w", owner, repo, number, res.err)
	}
	pr := res.pr
	if pr == nil {
		return nil, fmt.Errorf("extract pr.* facts for %s/%s#%d: no pull request returned", owner, repo, number)
	}

	// checks_pending is derived from the whole round of CI on the head sha, both
	// check runs and commit statuses — a repo can use either, or neither.
	checks, err := src.CommitChecks(ctx, owner, repo, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("extract pr.checks_pending for %s/%s#%d: %w", owner, repo, number, err)
	}

	diff, truncated, err := src.Diff(ctx, owner, repo, number, github.DiffMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("extract pr.diff for %s/%s#%d: %w", owner, repo, number, err)
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
	s.Bool("pr.checks_pending", checks.Pending())
	// pr.diff is the whole patch set, capped at github.DiffMaxBytes. Both facts
	// are always asserted: a PR with no textual changes gets an empty diff and
	// truncated false, which is honest — there is nothing to show. A diff the cap
	// cut off gets truncated true so it never reads as complete (issue #9).
	s.String("pr.diff", diff)
	s.Bool("pr.diff_truncated", truncated)
	// mergeable is the one fact GitHub computes asynchronously and returns null
	// for until it has — the common case right after a push, which is when a run
	// fires. A nil here is "GitHub has not said yet", which is omitted rather
	// than asserted false: "we do not know" is not "there are conflicts"
	// (facts.md, "pr.mergeable").
	if pr.Mergeable != nil {
		s.Bool("pr.mergeable", *pr.Mergeable)
	}
	return s, nil
}
