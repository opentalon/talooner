// Package assignment performs the two verbs that hand a pull request to a
// person: `do assign "pr" <user>` and `do require <review.target>`.
//
// Both are reversible (actions.md, "Reversibility"), and reversing them is the
// whole difficulty. GitHub reports an assignee Talooner added and one a
// maintainer added identically, so "remove the assignees the rules no longer ask
// for" has no answer in the API — a run that removed everything not in this
// run's verdict would quietly undo somebody's deliberate act on the first push
// after they made it.
//
// So Talooner keeps a ledger of what it added, in a sticky comment of its own
// (comment.TopicState), and takes back only what the ledger says is its. The
// consequences are deliberate:
//
//   - a deleted or unparsable ledger means Talooner owns nothing, so it removes
//     nothing. The failure direction is a stale assignee, never a removed one;
//   - a review request a human has already fulfilled is not in the PR's
//     requested reviewers at all — GitHub drops it when the review lands — so
//     withdrawal skips it without having to know it happened;
//   - an assignee GitHub silently ignored (no repo access, or the eleventh on a
//     PR) is detected by reading the response back, because a write that was
//     reported as a success and did nothing is the failure nobody would notice.
//
// Retraction is the absence of an action, which no executor can be handed, so
// the shape here is review's: the writer is built from the whole action set, the
// registry entry performs the positive half, and run calls Sync unconditionally.
package assignment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/comment"
	"github.com/opentalon/talooner/internal/github"
)

var (
	// ErrTarget is an assign or require argument that names nobody GitHub can be
	// asked about: an unmapped logical team, a team where only a user works, a
	// login with characters no login has.
	ErrTarget = errors.New("unresolvable target")
	// ErrIgnored is an assignee GitHub accepted and then left out of the pull
	// request. It is its own error because it is not a transport failure: the
	// call succeeded and the assignment did not happen.
	ErrIgnored = errors.New("assignee was ignored by github")
)

// namePattern is what a GitHub login or team slug may look like. It is a
// narrower set than either allows in theory, and it is also what keeps a ruleset
// from writing `-->` into the ledger comment and ending it early.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Set is one run's desired state: who should be assigned, and who should have a
// review request standing. Teams are slugs in the repository's own organisation.
type Set struct {
	Assignees []string
	Users     []string
	Teams     []string
}

// Writer is the GitHub half of a client, so run can pass its client and the
// tests a fake.
type Writer interface {
	CommentBody(ctx context.Context, owner, repo string, number int, marker string) (string, error)
	UpsertComment(ctx context.Context, owner, repo string, number int, s github.StickyComment) (int64, error)
	AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error)
	RemoveAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error)
	RequestReviewers(ctx context.Context, owner, repo string, number int, users, teams []string) (github.Reviewers, error)
	RemoveReviewRequests(ctx context.Context, owner, repo string, number int, users, teams []string) (github.Reviewers, error)
}

// Syncer performs one run's assignments and review requests, once.
type Syncer struct {
	gh      Writer
	owner   string
	repo    string
	number  int
	current github.Reviewers // review requests standing before this run
	held    []string         // assignees standing before this run
	want    Set
	log     *slog.Logger
	done    bool
}

// New builds the syncer for one decision. It resolves every assign and require
// argument up front, so a ruleset naming a team nothing maps to fails the run
// before any part of the verdict has been published — the same reason
// Registry.Validate runs before the sticky comment goes out.
//
// pr supplies the state to reconcile against, read from the fetch the run has
// already made rather than from calls of its own.
func New(gh Writer, owner, repo string, number int, pr *github.PullRequest,
	actions []action.Action, log *slog.Logger,
) (*Syncer, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if pr == nil {
		return nil, errors.New("assignment needs the pull request it is reconciling")
	}
	want, err := Desired(actions, pr.Author, log)
	if err != nil {
		return nil, err
	}
	return &Syncer{
		gh: gh, owner: owner, repo: repo, number: number,
		current: pr.Requested,
		held:    pr.Assignees,
		want:    want,
		log:     log,
	}, nil
}

// Desired is the state an action set asks for. Duplicates collapse: the same
// team required by two rules is one review request.
//
// The pull request's author is dropped from the review requests rather than
// sent: GitHub rejects the whole call with a 422 when one reviewer is the
// author, so asking would lose the other reviewers too.
func Desired(actions []action.Action, author string, log *slog.Logger) (Set, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var set Set
	for _, a := range actions {
		switch a.Verb {
		case action.VerbAssign:
			login, err := ResolveAssignee(a.Assignee)
			if err != nil {
				return Set{}, err
			}
			set.Assignees = add(set.Assignees, login)
		case action.VerbRequire:
			t, err := ResolveReviewer(a.Target)
			if err != nil {
				return Set{}, err
			}
			if t.Team {
				set.Teams = add(set.Teams, t.Name)
				continue
			}
			if strings.EqualFold(t.Name, author) {
				// Not an error: the rule is right that this PR needs that
				// reviewer, and GitHub simply will not ask an author to review
				// their own pull request.
				log.Warn("not requesting a review from the pull request's own author",
					"target", a.Target, "login", t.Name)
				continue
			}
			set.Users = add(set.Users, t.Name)
		}
	}
	return set, nil
}

// ResolveAssignee resolves an assign argument to a login. A team is an error rather
// than a silent skip: GitHub cannot assign one, so `do assign "pr" "org/team"`
// would otherwise read as a rule that fired and did nothing.
func ResolveAssignee(raw string) (string, error) {
	name := strings.TrimPrefix(strings.TrimSpace(raw), "@")
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: assign %q names a team, and github assigns people, not teams", ErrTarget, raw)
	}
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("%w: assign %q is not a github login", ErrTarget, raw)
	}
	return name, nil
}

// Target is a resolved require argument: a team slug in the repository's
// organisation, or a user's login.
type Target struct {
	Name string
	Team bool
}

// ResolveReviewer resolves a require target to a team slug or a login.
//
// Until E1 lands `.github/talooner/teams.yaml`, the mapping is the target's own
// last segment: review.security is the team `security` in the repository's
// organisation, and review.@alice is the user alice. E1 puts the file in front
// of this as an override layer, so a ruleset written today keeps working; a
// target this cannot resolve is an error naming it, never a skipped action.
func ResolveReviewer(raw string) (Target, error) {
	target := strings.TrimSpace(raw)
	rest := strings.TrimPrefix(target, "review.")
	if rest == "" {
		return Target{}, fmt.Errorf("%w: require needs a target", ErrTarget)
	}
	if user, ok := strings.CutPrefix(rest, "@"); ok {
		if !namePattern.MatchString(user) {
			return Target{}, fmt.Errorf("%w: require %q is not a github login", ErrTarget, raw)
		}
		return Target{Name: user}, nil
	}
	if !namePattern.MatchString(rest) || strings.Contains(rest, ".") {
		return Target{}, fmt.Errorf("%w: require %q maps to no team or user", ErrTarget, raw)
	}
	return Target{Name: rest, Team: true}, nil
}

// Execute performs assign and require. Both verbs map to the same syncer and
// the same single reconciliation: two assigns and a require in one verdict are
// one pass over GitHub, not three.
func (s *Syncer) Execute(ctx context.Context, a action.Action) error {
	if a.Verb != action.VerbAssign && a.Verb != action.VerbRequire {
		return fmt.Errorf("%w: the assignment syncer does not perform %s", action.ErrUnknownVerb, a.Verb)
	}
	return s.Sync(ctx)
}

// Sync makes the pull request's assignees and review requests match this run's
// verdict, once per run. run calls it whether or not either verb fired, because
// a decision with neither in it still has to take back what the last one added.
func (s *Syncer) Sync(ctx context.Context) error {
	if s.done {
		return nil
	}
	s.done = true

	// The ledger is read on every run, including the one where nothing is
	// standing and nothing is asked for. A claim on somebody who is not on the
	// pull request has to be dropped rather than left: otherwise a person
	// assigning that same login later would have Talooner take it away, which is
	// the one outcome this whole package exists to prevent.
	prev := s.ledger(ctx)
	assigned := clone(s.held)
	standing := github.Reviewers{Users: clone(s.current.Users), Teams: clone(s.current.Teams)}
	owned := prev
	var problems []error

	if addLogins := missing(s.want.Assignees, assigned); len(addLogins) > 0 {
		recorded, err := s.gh.AddAssignees(ctx, s.owner, s.repo, s.number, addLogins)
		switch {
		case err != nil:
			problems = append(problems, err)
		default:
			assigned = recorded
			landed := intersect(addLogins, recorded)
			owned.Assignees = union(owned.Assignees, landed)
			if ignored := missing(addLogins, recorded); len(ignored) > 0 {
				// GitHub returns 201 and drops these. Reporting it is the whole
				// point: a rule that assigned nobody looks exactly like a rule
				// that never fired.
				problems = append(problems, fmt.Errorf("%w: %s (no write access to %s/%s, or the pull request is at github's ten-assignee cap)",
					ErrIgnored, strings.Join(ignored, ", "), s.owner, s.repo))
			}
			s.log.Info("assignees added", "repo", s.owner+"/"+s.repo, "pr", s.number, "logins", landed)
		}
	}

	if drop := intersect(missing(owned.Assignees, s.want.Assignees), assigned); len(drop) > 0 {
		recorded, err := s.gh.RemoveAssignees(ctx, s.owner, s.repo, s.number, drop)
		if err != nil {
			problems = append(problems, err)
		} else {
			assigned = recorded
			owned.Assignees = missing(owned.Assignees, drop)
			s.log.Info("assignees removed", "repo", s.owner+"/"+s.repo, "pr", s.number, "logins", drop)
		}
	}

	addUsers := missing(s.want.Users, standing.Users)
	addTeams := missing(s.want.Teams, standing.Teams)
	if len(addUsers)+len(addTeams) > 0 {
		recorded, err := s.gh.RequestReviewers(ctx, s.owner, s.repo, s.number, addUsers, addTeams)
		if err != nil {
			problems = append(problems, err)
		} else {
			standing = recorded
			owned.Users = union(owned.Users, intersect(addUsers, recorded.Users))
			owned.Teams = union(owned.Teams, intersect(addTeams, recorded.Teams))
			s.log.Info("reviews requested", "repo", s.owner+"/"+s.repo, "pr", s.number,
				"users", addUsers, "teams", addTeams)
		}
	}

	// A request the reviewer already answered is gone from the standing list, so
	// intersecting with it is what leaves a completed review alone.
	dropUsers := intersect(missing(owned.Users, s.want.Users), standing.Users)
	dropTeams := intersect(missing(owned.Teams, s.want.Teams), standing.Teams)
	if len(dropUsers)+len(dropTeams) > 0 {
		recorded, err := s.gh.RemoveReviewRequests(ctx, s.owner, s.repo, s.number, dropUsers, dropTeams)
		if err != nil {
			problems = append(problems, err)
		} else {
			standing = recorded
			owned.Users = missing(owned.Users, dropUsers)
			owned.Teams = missing(owned.Teams, dropTeams)
			s.log.Info("review requests withdrawn", "repo", s.owner+"/"+s.repo, "pr", s.number,
				"users", dropUsers, "teams", dropTeams)
		}
	}

	// The ledger records what is true after the writes, not what was intended
	// before them: an assignee that never landed is not Talooner's to take away
	// later, and one a human has since removed is nothing to keep claiming.
	next := Ledger{
		Assignees: intersect(owned.Assignees, assigned),
		Users:     intersect(owned.Users, standing.Users),
		Teams:     intersect(owned.Teams, standing.Teams),
	}
	if err := s.writeLedger(ctx, prev, next); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

// ledger reads what the last run recorded. Every failure here — a listing that
// broke, a body somebody edited into nonsense — reads as an empty ledger and a
// warning, which means this run adds what the rules ask for and removes nothing.
// That is the safe direction: the cost is a stale assignee, and the alternative
// cost is taking away one a person added.
func (s *Syncer) ledger(ctx context.Context) Ledger {
	body, err := s.gh.CommentBody(ctx, s.owner, s.repo, s.number, comment.Marker(comment.TopicState))
	if err != nil {
		s.log.Warn("cannot read the assignment ledger, this run will remove nothing",
			"repo", s.owner+"/"+s.repo, "pr", s.number, "err", err)
		return Ledger{}
	}
	l, err := ParseLedger(body)
	if err != nil {
		s.log.Warn("assignment ledger is unreadable, this run will remove nothing",
			"repo", s.owner+"/"+s.repo, "pr", s.number, "err", err)
		return Ledger{}
	}
	return l
}

// writeLedger stores next, unless it says the same thing prev did. A PATCH per
// push that changes nothing is a wasted call and an edit in somebody's timeline.
func (s *Syncer) writeLedger(ctx context.Context, prev, next Ledger) error {
	if next.equal(prev) {
		return nil
	}
	id, err := s.gh.UpsertComment(ctx, s.owner, s.repo, s.number, github.StickyComment{
		Marker: comment.Marker(comment.TopicState),
		Body:   LedgerBody(next),
		// An empty ledger on a pull request that never had one is nothing to
		// say. On one that did, it is an edit: the comment stays, saying it now
		// tracks nothing, because a deleted comment takes its history with it.
		EditOnly: next.isEmpty(),
	})
	if err != nil {
		return fmt.Errorf("write the assignment ledger on %s/%s#%d: %w", s.owner, s.repo, s.number, err)
	}
	if id != 0 {
		s.log.Info("assignment ledger written", "repo", s.owner+"/"+s.repo, "pr", s.number, "id", id)
	}
	return nil
}

// add appends name unless it is already there, case-insensitively: GitHub
// logins are not case-sensitive, and assigning Alice twice is one assignment.
func add(list []string, name string) []string {
	for _, got := range list {
		if strings.EqualFold(got, name) {
			return list
		}
	}
	return append(list, name)
}

// missing is the members of a that are not in b.
func missing(a, b []string) []string {
	var out []string
	for _, want := range a {
		if !contains(b, want) {
			out = append(out, want)
		}
	}
	return out
}

// intersect is the members of a that are also in b, spelled the way a has them.
func intersect(a, b []string) []string {
	var out []string
	for _, want := range a {
		if contains(b, want) {
			out = append(out, want)
		}
	}
	return out
}

func union(a, b []string) []string {
	out := clone(a)
	for _, name := range b {
		out = add(out, name)
	}
	return out
}

func contains(list []string, name string) bool {
	for _, got := range list {
		if strings.EqualFold(got, name) {
			return true
		}
	}
	return false
}

func clone(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	copy(out, list)
	return out
}
