package facts

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

type Source interface {
	ResolveMergeable(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error)
	ChangedFileStats(ctx context.Context, owner, repo string, number int) ([]github.FileStat, error)
	CommitChecks(ctx context.Context, owner, repo, headSHA string) (github.Checks, error)
	Diff(ctx context.Context, owner, repo string, number, maxBytes int) (string, bool, error)
	PullRequestReviews(ctx context.Context, owner, repo string, number int) ([]github.ReviewReport, error)
	LastToucher(ctx context.Context, owner, repo, baseSHA string, paths []string) (string, error)
}

func PR(ctx context.Context, src Source, owner, repo string, number int, checks config.Checks, codeowners []byte, modules []config.Module, teams config.Teams, arch []config.ArchitectureRule) (Set, []CodeUnit, error) {
	type prResult struct {
		pr  *github.PullRequest
		err error
	}
	prCh := make(chan prResult, 1)
	resolveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		pr, err := src.ResolveMergeable(resolveCtx, owner, repo, number)
		prCh <- prResult{pr, err}
	}()

	stats, err := src.ChangedFileStats(ctx, owner, repo, number)
	if err != nil {
		return nil, nil, fmt.Errorf("extract pr.changed_files for %s/%s#%d: %w", owner, repo, number, err)
	}
	changed := make([]string, 0, len(stats))
	for _, f := range stats {
		changed = append(changed, f.Path)
	}

	res := <-prCh
	if res.err != nil {
		return nil, nil, fmt.Errorf("extract pr.* facts for %s/%s#%d: %w", owner, repo, number, res.err)
	}
	pr := res.pr
	if pr == nil {
		return nil, nil, fmt.Errorf("extract pr.* facts for %s/%s#%d: no pull request returned", owner, repo, number)
	}

	ci, err := src.CommitChecks(ctx, owner, repo, pr.HeadSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("extract pr.checks_pending for %s/%s#%d: %w", owner, repo, number, err)
	}

	diff, truncated, err := src.Diff(ctx, owner, repo, number, github.DiffMaxBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("extract pr.diff for %s/%s#%d: %w", owner, repo, number, err)
	}

	reviews, err := src.PullRequestReviews(ctx, owner, repo, number)
	if err != nil {
		return nil, nil, fmt.Errorf("extract review.* for %s/%s#%d: %w", owner, repo, number, err)
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
	s.Bool("pr.checks_pending", ci.Pending())
	s.String("pr.diff", diff)
	s.Bool("pr.diff_truncated", truncated)
	newDeps, upgradedDeps, err := countDependencyChanges(diff, stats)
	if err != nil {
		return nil, nil, fmt.Errorf("extract pr.new_dependencies / pr.upgraded_dependencies for %s/%s#%d: %w", owner, repo, number, err)
	}
	s.Int("pr.new_dependencies", newDeps)
	s.Int("pr.upgraded_dependencies", upgradedDeps)
	if pr.Mergeable != nil {
		s.Bool("pr.mergeable", *pr.Mergeable)
	}

	if v := derivePassing(ci.Runs, ci.Statuses, checks.Tests); v != nil {
		s.Bool("pr.tests_passing", *v)
	}
	if v := derivePassing(ci.Runs, ci.Statuses, checks.Lint); v != nil {
		s.Bool("pr.lint_passing", *v)
	}

	if err := userFacts(ctx, src, s, owner, repo, pr, changed, codeowners); err != nil {
		return nil, nil, fmt.Errorf("extract user.owner for %s/%s#%d: %w", owner, repo, number, err)
	}
	moduleFacts(s, stats, modules)
	units := architectureFacts(s, stats, diff, arch)
	reviewFacts(s, pr.HeadSHA, reviews, changed, codeowners, teams, pr.Requested.Teams, owner)
	return s, units, nil
}

func userFacts(ctx context.Context, src Source, s Set, owner, repo string, pr *github.PullRequest, changed []string, codeowners []byte) error {
	s.String("user.author", pr.Author)

	if r := pr.Requested.Users; len(r) > 0 {
		s.String("user.reviewer", r[0])
	} else if t := pr.Requested.Teams; len(t) > 0 {
		s.String("user.reviewer", t[0])
	}

	if len(codeowners) > 0 {
		if primary, owners := resolveOwners(parseCodeowners(codeowners), changed); owners != nil {
			s.String("user.owner", primary)
			s.Strings("user.owners", owners)
			return nil
		}
	}

	toucher, err := src.LastToucher(ctx, owner, repo, pr.BaseSHA, changed)
	if err != nil {
		return fmt.Errorf("last toucher: %w", err)
	}
	if toucher != "" {
		s.String("user.owner", toucher)
		s.Strings("user.owners", []string{toucher})
		s.String("user.last_toucher", toucher)
	}
	return nil
}

func derivePassing(runs []github.CheckRunReport, statuses []github.CommitStatus, patterns []string) *bool {
	if len(patterns) == 0 {
		return nil
	}
	matched, pending, failed, unknown := false, false, false, false
	for _, r := range runs {
		if !matchAny(patterns, r.Name) {
			continue
		}
		matched = true
		switch r.Status {
		case "queued", "in_progress":
			pending = true
		default:
			switch r.Conclusion {
			case "success":
			case "failure", "timed_out", "cancelled":
				failed = true
			default:
				unknown = true
			}
		}
	}
	for _, s := range statuses {
		if !matchAny(patterns, s.Context) {
			continue
		}
		matched = true
		switch s.State {
		case "pending":
			pending = true
		case "success":
		case "failure", "error":
			failed = true
		default:
			unknown = true
		}
	}
	if !matched || pending {
		return nil
	}
	if failed {
		return boolPtr(false)
	}
	if unknown {
		return nil
	}
	return boolPtr(true)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchPattern(p, name) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, name string) bool {
	lower := strings.ToLower(name)
	pat := strings.ToLower(pattern)
	if !strings.Contains(pat, "*") {
		return pat == lower
	}
	var b strings.Builder
	b.WriteString("^")
	for {
		before, after, found := strings.Cut(pat, "*")
		b.WriteString(regexp.QuoteMeta(before))
		if !found {
			break
		}
		b.WriteString(".*")
		pat = after
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(lower)
}

func boolPtr(b bool) *bool { return &b }
