package assignment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/comment"
	"github.com/opentalon/talooner/internal/github"
)

// fakeGitHub is the pull request as GitHub holds it, plus a record of every
// write. It applies the writes, so a test asserts the state a run left behind
// rather than only the calls it made.
type fakeGitHub struct {
	assignees []string
	users     []string
	teams     []string
	ledger    string

	// ignored is the logins GitHub accepts and silently drops: no write access,
	// or the eleventh assignee.
	ignored []string
	// failures, one per call, so a half-failed sync is testable.
	addAssigneesErr, removeAssigneesErr, requestErr, unrequestErr, ledgerReadErr, ledgerWriteErr error

	calls   []string
	written []github.StickyComment
}

func (f *fakeGitHub) CommentBody(_ context.Context, _, _ string, _ int, marker string) (string, error) {
	f.calls = append(f.calls, "read ledger")
	if f.ledgerReadErr != nil {
		return "", f.ledgerReadErr
	}
	if !strings.Contains(f.ledger, marker) {
		return "", nil
	}
	return f.ledger, nil
}

func (f *fakeGitHub) UpsertComment(_ context.Context, _, _ string, _ int, s github.StickyComment) (int64, error) {
	f.calls = append(f.calls, "write ledger")
	if f.ledgerWriteErr != nil {
		return 0, f.ledgerWriteErr
	}
	f.written = append(f.written, s)
	if s.EditOnly && f.ledger == "" {
		return 0, nil
	}
	f.ledger = s.Marker + "\n" + s.Body
	return 1, nil
}

func (f *fakeGitHub) AddAssignees(_ context.Context, _, _ string, _ int, logins []string) ([]string, error) {
	f.calls = append(f.calls, "add assignees "+strings.Join(logins, ","))
	if f.addAssigneesErr != nil {
		return nil, f.addAssigneesErr
	}
	for _, login := range logins {
		if !contains(f.ignored, login) {
			f.assignees = add(f.assignees, login)
		}
	}
	return clone(f.assignees), nil
}

func (f *fakeGitHub) RemoveAssignees(_ context.Context, _, _ string, _ int, logins []string) ([]string, error) {
	f.calls = append(f.calls, "remove assignees "+strings.Join(logins, ","))
	if f.removeAssigneesErr != nil {
		return nil, f.removeAssigneesErr
	}
	f.assignees = missing(f.assignees, logins)
	return clone(f.assignees), nil
}

func (f *fakeGitHub) RequestReviewers(_ context.Context, _, _ string, _ int, users, teams []string) (github.Reviewers, error) {
	f.calls = append(f.calls, "request "+strings.Join(append(clone(users), teams...), ","))
	if f.requestErr != nil {
		return github.Reviewers{}, f.requestErr
	}
	f.users = union(f.users, users)
	f.teams = union(f.teams, teams)
	return f.standing(), nil
}

func (f *fakeGitHub) RemoveReviewRequests(_ context.Context, _, _ string, _ int, users, teams []string) (github.Reviewers, error) {
	f.calls = append(f.calls, "unrequest "+strings.Join(append(clone(users), teams...), ","))
	if f.unrequestErr != nil {
		return github.Reviewers{}, f.unrequestErr
	}
	f.users = missing(f.users, users)
	f.teams = missing(f.teams, teams)
	return f.standing(), nil
}

func (f *fakeGitHub) standing() github.Reviewers {
	return github.Reviewers{Users: clone(f.users), Teams: clone(f.teams)}
}

// pr is the pull request as the run fetched it, i.e. before this run's writes.
func (f *fakeGitHub) pr(author string) *github.PullRequest {
	return &github.PullRequest{
		Number: 42, Author: author,
		Assignees: clone(f.assignees),
		Requested: f.standing(),
	}
}

func (f *fakeGitHub) ledgerOf(t *testing.T) Ledger {
	t.Helper()
	l, err := ParseLedger(f.ledger)
	if err != nil {
		t.Fatalf("ParseLedger(%q): %v", f.ledger, err)
	}
	return l
}

func ledgerComment(l Ledger) string {
	return comment.Marker(comment.TopicState) + "\n" + LedgerBody(l)
}

func assign(login string) action.Action {
	return action.Action{Verb: action.VerbAssign, Target: "pr", Assignee: login}
}

func require(target string) action.Action {
	return action.Action{Verb: action.VerbRequire, Target: target}
}

func sync(t *testing.T, f *fakeGitHub, author string, actions ...action.Action) error {
	t.Helper()
	s, err := New(f, "opentalon", "talooner", 42, f.pr(author), actions, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Sync(t.Context())
}

func TestAssignAndRequireWriteAndAreRecorded(t *testing.T) {
	f := &fakeGitHub{}

	if err := sync(t, f, "evgeny", assign("@alice"), require("review.security"), require("review.@bob")); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if want := []string{"alice"}; !equalSets(f.assignees, want) {
		t.Errorf("assignees = %v, want %v", f.assignees, want)
	}
	if want := []string{"bob"}; !equalSets(f.users, want) {
		t.Errorf("requested users = %v, want %v", f.users, want)
	}
	if want := []string{"security"}; !equalSets(f.teams, want) {
		t.Errorf("requested teams = %v, want %v", f.teams, want)
	}

	got := f.ledgerOf(t)
	if !equalSets(got.Assignees, []string{"alice"}) || !equalSets(got.Users, []string{"bob"}) ||
		!equalSets(got.Teams, []string{"security"}) {
		t.Errorf("ledger = %+v, want everything this run added", got)
	}
}

// The ledger is the whole ownership rule: what a person put on the PR stays
// there, however long the rules have gone without asking for it.
func TestRetractionRemovesOnlyWhatTheLedgerClaims(t *testing.T) {
	f := &fakeGitHub{
		assignees: []string{"alice", "carol"},
		users:     []string{"bob", "dave"},
		teams:     []string{"security", "design"},
		ledger:    ledgerComment(Ledger{Assignees: []string{"alice"}, Users: []string{"bob"}, Teams: []string{"security"}}),
	}

	// No actions at all: the rules stopped asking for any of it.
	if err := sync(t, f, "evgeny"); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if want := []string{"carol"}; !equalSets(f.assignees, want) {
		t.Errorf("assignees = %v, want only the human's %v", f.assignees, want)
	}
	if want := []string{"dave"}; !equalSets(f.users, want) {
		t.Errorf("requested users = %v, want only the human's %v", f.users, want)
	}
	if want := []string{"design"}; !equalSets(f.teams, want) {
		t.Errorf("requested teams = %v, want only the human's %v", f.teams, want)
	}
	if got := f.ledgerOf(t); !got.isEmpty() {
		t.Errorf("ledger = %+v, want empty: Talooner is holding nothing now", got)
	}
}

// An unreadable ledger is the case where Talooner cannot tell its own
// assignments from a person's. It must then remove nothing at all.
func TestAnUnreadableLedgerRemovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ledger string
		err    error
	}{
		{name: "deleted", ledger: ""},
		{name: "edited into nonsense", ledger: comment.Marker(comment.TopicState) + "\n<!-- talooner-ledger {oops -->\n"},
		{name: "unclosed", ledger: comment.Marker(comment.TopicState) + "\n<!-- talooner-ledger {\"assignees\":[\"alice\"]}\n"},
		{name: "listing failed", ledger: "", err: errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitHub{assignees: []string{"alice"}, ledger: tc.ledger, ledgerReadErr: tc.err}

			if err := sync(t, f, "evgeny"); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if want := []string{"alice"}; !equalSets(f.assignees, want) {
				t.Errorf("assignees = %v, want %v left alone", f.assignees, want)
			}
			for _, call := range f.calls {
				if strings.HasPrefix(call, "remove") || strings.HasPrefix(call, "unrequest") {
					t.Errorf("call %q: an unreadable ledger must take nothing away", call)
				}
			}
		})
	}
}

// GitHub answers 201 and drops an assignee the login cannot be given. A rule
// that assigned nobody looks exactly like a rule that never fired, so the run
// fails rather than reporting a success.
func TestAnIgnoredAssigneeFailsTheRun(t *testing.T) {
	f := &fakeGitHub{ignored: []string{"stranger"}}

	err := sync(t, f, "evgeny", assign("stranger"), assign("alice"))
	if !errors.Is(err, ErrIgnored) {
		t.Fatalf("err = %v, want ErrIgnored", err)
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("err = %v, want it to name the login github dropped", err)
	}
	// The one that did land is still Talooner's to take back later.
	if got := f.ledgerOf(t); !equalSets(got.Assignees, []string{"alice"}) {
		t.Errorf("ledger = %+v, want only the assignee that actually landed", got)
	}
}

// GitHub drops a review request when the review is submitted, so a fulfilled
// request is simply not standing. Withdrawal must not go looking for it.
func TestAFulfilledRequestIsNotWithdrawn(t *testing.T) {
	f := &fakeGitHub{
		users:  nil, // bob reviewed, so github already dropped the request
		ledger: ledgerComment(Ledger{Users: []string{"bob"}}),
	}

	if err := sync(t, f, "evgeny"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "unrequest") {
			t.Errorf("call %q: the request was already fulfilled", call)
		}
	}
}

// A rule that keeps firing is not a write on every push: an email per push is
// the cost, and nothing about the PR changed.
func TestNothingIsWrittenWhenTheStateAlreadyMatches(t *testing.T) {
	f := &fakeGitHub{
		assignees: []string{"alice"},
		teams:     []string{"security"},
		ledger:    ledgerComment(Ledger{Assignees: []string{"alice"}, Teams: []string{"security"}}),
	}

	if err := sync(t, f, "evgeny", assign("alice"), require("review.security")); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if want := []string{"read ledger"}; !equalSets(f.calls, want) {
		t.Errorf("calls = %v, want %v: nothing changed", f.calls, want)
	}
}

// The common run has no assign, no require and nothing standing. It costs the
// one ledger read and writes nothing: a pull request Talooner never touched
// does not get a comment saying so.
func TestAnEmptyPullRequestOnlyReadsTheLedger(t *testing.T) {
	f := &fakeGitHub{}

	if err := sync(t, f, "evgeny"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if want := []string{"read ledger"}; !equalSets(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if len(f.written) != 0 {
		t.Errorf("wrote %+v, want no ledger comment at all", f.written)
	}
}

// A human who unassigned somebody Talooner added has said something. Talooner
// re-asserts the rule — it is still firing — but the interesting half is that
// the ledger does not claim what is not there.
func TestALedgerEntryAHumanRemovedIsNotClaimed(t *testing.T) {
	f := &fakeGitHub{
		assignees: nil,
		ledger:    ledgerComment(Ledger{Assignees: []string{"alice"}}),
	}

	if err := sync(t, f, "evgeny"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := f.ledgerOf(t); !got.isEmpty() {
		t.Errorf("ledger = %+v, want empty: there is nothing left to own", got)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "remove") {
			t.Errorf("call %q: alice was already gone", call)
		}
	}
}

// GitHub 422s the whole review-request call when one reviewer is the PR's own
// author, which would lose the other reviewers with it.
func TestTheAuthorIsNeverAskedToReviewTheirOwnPullRequest(t *testing.T) {
	f := &fakeGitHub{}

	if err := sync(t, f, "evgeny", require("review.@evgeny"), require("review.@bob")); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if want := []string{"bob"}; !equalSets(f.users, want) {
		t.Errorf("requested users = %v, want %v", f.users, want)
	}
}

// A write that failed is reported, and the ledger still records the truth: the
// next run must not believe it owns something that never landed.
func TestAFailedWriteIsReportedAndNotRecorded(t *testing.T) {
	f := &fakeGitHub{requestErr: errors.New("boom")}

	err := sync(t, f, "evgeny", assign("alice"), require("review.security"))
	if err == nil {
		t.Fatal("Sync = nil, want the failed review request")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the underlying failure", err)
	}
	got := f.ledgerOf(t)
	if !equalSets(got.Assignees, []string{"alice"}) {
		t.Errorf("ledger assignees = %v, want the write that succeeded", got.Assignees)
	}
	if len(got.Teams) != 0 {
		t.Errorf("ledger teams = %v, want none: the request never landed", got.Teams)
	}
}

// Sync is the reconciliation, and the registry calls it too. Two verbs in one
// verdict must not mean two passes over GitHub.
func TestSyncHappensOnceHoweverManyVerbsFired(t *testing.T) {
	f := &fakeGitHub{}
	s, err := New(f, "opentalon", "talooner", 42, f.pr("evgeny"),
		[]action.Action{assign("alice"), require("review.security")}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Execute(t.Context(), assign("alice")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := s.Execute(t.Context(), require("review.security")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := s.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	var adds int
	for _, call := range f.calls {
		if strings.HasPrefix(call, "add assignees") {
			adds++
		}
	}
	if adds != 1 {
		t.Errorf("added assignees %d times, want once", adds)
	}
}

func TestExecuteRefusesAVerbItDoesNotPerform(t *testing.T) {
	f := &fakeGitHub{}
	s, err := New(f, "opentalon", "talooner", 42, f.pr("evgeny"), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = s.Execute(t.Context(), action.Action{Verb: action.VerbApprove, Target: "pr"})
	if !errors.Is(err, action.ErrUnknownVerb) {
		t.Errorf("err = %v, want ErrUnknownVerb", err)
	}
}

func TestResolveAssignee(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		bad  bool
	}{
		{raw: "alice", want: "alice"},
		{raw: "@alice", want: "alice"},
		{raw: " @alice ", want: "alice"},
		{raw: "org/security", bad: true}, // github assigns people, not teams
		{raw: "", bad: true},             // the fact it resolved from was unset
		{raw: "@", bad: true},
		{raw: "alice bob", bad: true},
		{raw: "-alice", bad: true},
		{raw: "alice-->", bad: true}, // would end the ledger comment early
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ResolveAssignee(tc.raw)
			if tc.bad {
				if !errors.Is(err, ErrTarget) {
					t.Fatalf("ResolveAssignee(%q) = %q, %v, want ErrTarget", tc.raw, got, err)
				}
				if !strings.Contains(err.Error(), "assign") {
					t.Errorf("err = %v, want it to name the verb", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ResolveAssignee(%q) = %q, %v, want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestResolveReviewer(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want Target
		bad  bool
	}{
		{raw: "review.security", want: Target{Name: "security", Team: true}},
		{raw: "review.@alice", want: Target{Name: "alice"}},
		{raw: "security", want: Target{Name: "security", Team: true}},
		{raw: "review.foo.bar", bad: true}, // nothing maps this
		{raw: "review.", bad: true},
		{raw: "", bad: true},
		{raw: "review.@", bad: true},
		{raw: "review.a-->b", bad: true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ResolveReviewer(tc.raw)
			if tc.bad {
				if !errors.Is(err, ErrTarget) {
					t.Fatalf("ResolveReviewer(%q) = %+v, %v, want ErrTarget", tc.raw, got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ResolveReviewer(%q) = %+v, %v, want %+v", tc.raw, got, err, tc.want)
			}
		})
	}
}

// An unresolvable target fails at construction, before the run has written any
// part of the verdict.
func TestAnUnresolvableTargetFailsBeforeAnyWrite(t *testing.T) {
	f := &fakeGitHub{}

	_, err := New(f, "opentalon", "talooner", 42, f.pr("evgeny"), []action.Action{require("review.foo.bar")}, nil)
	if !errors.Is(err, ErrTarget) {
		t.Fatalf("New = %v, want ErrTarget", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %v, want none", f.calls)
	}
}

func TestNewNeedsThePullRequest(t *testing.T) {
	if _, err := New(&fakeGitHub{}, "opentalon", "talooner", 42, nil, nil, nil); err == nil {
		t.Error("New = nil, want the missing pull request to be an error")
	}
}

// equalSets compares without regard to order, the way the ledger does.
func equalSets(a, b []string) bool { return sameSet(a, b) }
