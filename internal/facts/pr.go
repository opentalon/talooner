package facts

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/opentalon/talooner/internal/config"
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
	ChangedFileStats(ctx context.Context, owner, repo string, number int) ([]github.FileStat, error)
	CommitChecks(ctx context.Context, owner, repo, headSHA string) (github.Checks, error)
	// Diff is the concatenated file patches, capped at maxBytes. The second
	// return is whether the cap was hit, so a rule can tell a complete diff from
	// a truncated one (issue #9).
	Diff(ctx context.Context, owner, repo string, number, maxBytes int) (string, bool, error)
	// PullRequestReviews is every review ever submitted, unfolded — review.*
	// facts fold it to current state per login (facts.md, "review.*").
	PullRequestReviews(ctx context.Context, owner, repo string, number int) ([]github.ReviewReport, error)
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
//
// checks are the tenant's test/lint name patterns from config.yaml. They filter
// the CommitChecks fetch C8 already makes (facts.md, "tests_passing /
// lint_passing"): pr.tests_passing and pr.lint_passing are derived from it, so
// the two facts cost no extra API call. An empty pattern list leaves the
// matching fact unset rather than guessing.
//
// codeowners is the repo's CODEOWNERS file, read from the base branch at its own
// ref (facts.md, "user.owner"); it is nil when the repo has none. user.owner and
// user.owners are derived from it against the changed paths — the first tier of
// the owner resolution order. The modules.yaml and git-history tiers are later
// tickets, so a path CODEOWNERS does not cover is left unowned rather than
// guessed (see resolveOwners).
//
// modules is the repo's .github/talooner/modules.yaml (facts.md, "module.*"),
// read from the base branch like the ruleset so a fork PR cannot redefine what it
// touches. An empty slice means the repo declared no modules, so every module.*
// fact stays unset except the always-asserted module.touched_count, which reads 0
// (facts.md, "module.touched_count").
//
// teams is the repo's .github/talooner/teams.yaml (facts.md, "team.*"), read
// the same way; it is also what review.<team>.* asserts facts for, so a
// logical name a ruleset requires review from is the same name it reads
// review facts back on.
func PR(ctx context.Context, src Source, owner, repo string, number int, checks config.Checks, codeowners []byte, modules []config.Module, teams config.Teams) (Set, error) {
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

	stats, err := src.ChangedFileStats(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("extract pr.changed_files for %s/%s#%d: %w", owner, repo, number, err)
	}
	changed := make([]string, 0, len(stats))
	for _, f := range stats {
		changed = append(changed, f.Path)
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
	ci, err := src.CommitChecks(ctx, owner, repo, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("extract pr.checks_pending for %s/%s#%d: %w", owner, repo, number, err)
	}

	diff, truncated, err := src.Diff(ctx, owner, repo, number, github.DiffMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("extract pr.diff for %s/%s#%d: %w", owner, repo, number, err)
	}

	reviews, err := src.PullRequestReviews(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("extract review.* for %s/%s#%d: %w", owner, repo, number, err)
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
	// pr.diff is the whole patch set, capped at github.DiffMaxBytes. Both facts
	// are always asserted: a PR with no textual changes gets an empty diff and
	// truncated false, which is honest — there is nothing to show. A diff the cap
	// cut off gets truncated true so it never reads as complete (issue #9).
	s.String("pr.diff", diff)
	s.Bool("pr.diff_truncated", truncated)
	// pr.new_dependencies / pr.upgraded_dependencies are the counts of dependencies
	// added and version-bumped across recognised manifests, parsed from the diff
	// C2 already fetched (facts.md, "pr.new_dependencies",
	// "pr.upgraded_dependencies"). Both always asserted: a PR with none of either
	// gets 0, which is the honest answer, not a dead extractor. Lockfile churn is
	// excluded from both; a version bump counts toward upgraded, never new
	// (issue #11).
	newDeps, upgradedDeps := countDependencyChanges(diff)
	s.Int("pr.new_dependencies", newDeps)
	s.Int("pr.upgraded_dependencies", upgradedDeps)
	// mergeable is the one fact GitHub computes asynchronously and returns null
	// for until it has — the common case right after a push, which is when a run
	// fires. A nil here is "GitHub has not said yet", which is omitted rather
	// than asserted false: "we do not know" is not "there are conflicts"
	// (facts.md, "pr.mergeable").
	if pr.Mergeable != nil {
		s.Bool("pr.mergeable", *pr.Mergeable)
	}

	// pr.tests_passing / pr.lint_passing read the same check runs and statuses
	// C8 fetched for pr.checks_pending, filtered by the tenant's name patterns
	// (facts.md). Each is asserted only when it has a determined value; the
	// cases that leave it unset — no matching check, CI still running, or a
	// check with a conclusion this build does not recognise — are deliberate
	// omissions, not false, because a positive condition on an unset fact simply
	// does not fire (facts.md, "Unset is false").
	if v := derivePassing(ci.Runs, ci.Statuses, checks.Tests); v != nil {
		s.Bool("pr.tests_passing", *v)
	}
	if v := derivePassing(ci.Runs, ci.Statuses, checks.Lint); v != nil {
		s.Bool("pr.lint_passing", *v)
	}

	// user.* facts (facts.md, "user.*"): user.author is pr.author for symmetry,
	// user.reviewer the standing review request, user.owner / user.owners the
	// CODEOWNERS-derived ownership. All read data already fetched here; the
	// CODEOWNERS content is passed in rather than re-read.
	userFacts(s, pr, changed, codeowners)
	// module.* facts (facts.md, "module.*"): bound to the primary touched module
	// by most changed lines, with module.touched_count always asserted. The file
	// stats are already in hand from the ChangedFileStats fetch above.
	moduleFacts(s, stats, modules)
	// review.* facts (facts.md, "review.*"): folded from the whole review
	// history fetched above, against the touched paths and the team lookup
	// table already read for the require resolver.
	reviewFacts(s, pr.HeadSHA, reviews, changed, codeowners, teams, pr.Requested.Teams, owner)
	return s, nil
}

// userFacts asserts the user.* namespace (facts.md, "user.*") into s.
func userFacts(s Set, pr *github.PullRequest, changed []string, codeowners []byte) {
	// user.author aliases pr.author for symmetry. Always asserted: a rule quoting
	// user.author on a PR with no author is not a real case, but an omitted fact
	// there would read as a dead extractor (facts.md, "Unset is false").
	s.String("user.author", pr.Author)

	// user.reviewer is the one standing review request, if any. The PR carries
	// users and teams separately; a user login is preferred, then a team slug,
	// because a rule that tags the reviewer wants a person when one is asked. Left
	// unset when nothing is requested — that is the honest answer, not "".
	if r := pr.Requested.Users; len(r) > 0 {
		s.String("user.reviewer", r[0])
	} else if t := pr.Requested.Teams; len(t) > 0 {
		s.String("user.reviewer", t[0])
	}

	// user.owner / user.owners come from CODEOWNERS, the first tier of the owner
	// resolution order (facts.md). A repo without CODEOWNERS, or one whose
	// CODEOWNERS names no owner for any touched path, leaves both facts unset
	// rather than guessed at pr.author — the modules.yaml and git-history tiers
	// are later tickets.
	if len(codeowners) > 0 {
		if primary, owners := resolveOwners(parseCodeowners(codeowners), changed); owners != nil {
			s.String("user.owner", primary)
			s.Strings("user.owners", owners)
		}
	}
}

// derivePassing turns the head sha's CI into one pass-gate fact for a set of
// name patterns (tests or lint). It returns nil — the fact is left unset — when
// the patterns match no check, when any matched check is still running, or when
// any matched check has a conclusion neither success nor a recognised failure.
// All matched checks completed as success reads true; any matched check failed
// (or errored) reads false.
//
// Precedence, because a mixed set has to resolve one way: pending beats
// everything (a gate must not fire while CI is in flight); a recognised failure
// beats an unknown conclusion (a PR with one red test and one neutral test is
// not passing); an unknown conclusion beats success-only (we do not claim
// passing on a check whose outcome we cannot name). Failure winning over unknown
// is the one call here: the alternative — unset on any unknown — would let a red
// test go unblocked behind a neutral one.
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

// matchAny reports whether name matches any pattern. Patterns are
// case-insensitive wildcards where "*" matches any characters, including a
// slash, so "ci/*" matches "ci/build" and "*unit*" matches "my-unit-tests".
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
	// Build an anchored regexp from the pattern: "*" becomes ".*", everything
	// else is matched literally. Names are short and few, so compiling per call
	// is cheaper than caching and keeps this free of state.
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
		// An uncompilable pattern matches nothing rather than panicking a run.
		return false
	}
	return re.MatchString(lower)
}

func boolPtr(b bool) *bool { return &b }
