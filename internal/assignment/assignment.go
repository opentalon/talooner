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
	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

var (
	ErrTarget  = errors.New("unresolvable target")
	ErrIgnored = errors.New("assignee was ignored by github")
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

type Set struct {
	Assignees []string
	Users     []string
	Teams     []string
}

type Writer interface {
	CommentBody(ctx context.Context, owner, repo string, number int, marker string) (string, error)
	UpsertComment(ctx context.Context, owner, repo string, number int, s github.StickyComment) (int64, error)
	AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error)
	RemoveAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error)
	RequestReviewers(ctx context.Context, owner, repo string, number int, users, teams []string) (github.Reviewers, error)
	RemoveReviewRequests(ctx context.Context, owner, repo string, number int, users, teams []string) (github.Reviewers, error)
}

type Syncer struct {
	gh      Writer
	owner   string
	repo    string
	number  int
	current github.Reviewers
	held    []string
	want    Set
	log     *slog.Logger
	done    bool
}

func New(gh Writer, owner, repo string, number int, pr *github.PullRequest,
	actions []action.Action, teams config.Teams, log *slog.Logger,
) (*Syncer, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if pr == nil {
		return nil, errors.New("assignment needs the pull request it is reconciling")
	}
	want, err := Desired(actions, pr.Author, teams, log)
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

func Desired(actions []action.Action, author string, teams config.Teams, log *slog.Logger) (Set, error) {
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
			t, err := ResolveReviewer(a.Target, teams)
			if err != nil {
				return Set{}, err
			}
			if t.Team {
				set.Teams = add(set.Teams, t.Name)
				continue
			}
			if strings.EqualFold(t.Name, author) {
				log.Warn("not requesting a review from the pull request's own author",
					"target", a.Target, "login", t.Name)
				continue
			}
			set.Users = add(set.Users, t.Name)
		}
	}
	return set, nil
}

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

type Target struct {
	Name string
	Team bool
}

func ResolveReviewer(raw string, teams config.Teams) (Target, error) {
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
	if teams != nil {
		if handle, ok := teams[rest]; ok && handle != "" {
			return Target{Name: strings.TrimPrefix(handle, "@"), Team: true}, nil
		}
	}
	if !namePattern.MatchString(rest) || strings.Contains(rest, ".") {
		return Target{}, fmt.Errorf("%w: require %q maps to no team or user", ErrTarget, raw)
	}
	return Target{Name: rest, Team: true}, nil
}

func (s *Syncer) Execute(ctx context.Context, a action.Action) error {
	if a.Verb != action.VerbAssign && a.Verb != action.VerbRequire {
		return fmt.Errorf("%w: the assignment syncer does not perform %s", action.ErrUnknownVerb, a.Verb)
	}
	return s.Sync(ctx)
}

func (s *Syncer) Sync(ctx context.Context) error {
	if s.done {
		return nil
	}
	s.done = true

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

func (s *Syncer) writeLedger(ctx context.Context, prev, next Ledger) error {
	if next.equal(prev) {
		return nil
	}
	id, err := s.gh.UpsertComment(ctx, s.owner, s.repo, s.number, github.StickyComment{
		Marker:   comment.Marker(comment.TopicState),
		Body:     LedgerBody(next),
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

func add(list []string, name string) []string {
	for _, got := range list {
		if strings.EqualFold(got, name) {
			return list
		}
	}
	return append(list, name)
}

func missing(a, b []string) []string {
	var out []string
	for _, want := range a {
		if !contains(b, want) {
			out = append(out, want)
		}
	}
	return out
}

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
