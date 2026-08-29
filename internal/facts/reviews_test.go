package facts

import (
	"context"
	"errors"
	"testing"

	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/github"
)

func reviewPR() *github.PullRequest {
	pr := samplePR()
	pr.HeadSHA = "head2"
	return pr
}

// review.human.approved counts only a non-bot approval at the current head
// sha; a bot's approval never counts, and an approval at an old sha is
// re-derived as not-approved rather than assumed still standing.
func TestReviewHumanApproved(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reviews []github.ReviewReport
		want    bool
	}{
		{"no reviews", nil, false},
		{"human approves at head", []github.ReviewReport{
			{ID: 1, Login: "alice", State: github.StateApproved, CommitID: "head2"},
		}, true},
		{"bot approval does not count", []github.ReviewReport{
			{ID: 1, Login: "dependabot", Bot: true, State: github.StateApproved, CommitID: "head2"},
		}, false},
		{"approval at a stale sha does not count", []github.ReviewReport{
			{ID: 1, Login: "alice", State: github.StateApproved, CommitID: "head1"},
		}, false},
		{"changes requested is not an approval", []github.ReviewReport{
			{ID: 1, Login: "alice", State: github.StateChangesRequested, CommitID: "head2"},
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(), fakeSource{pr: reviewPR(), reviews: tt.reviews},
				"opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["review.human.approved"] != tt.want {
				t.Errorf("review.human.approved = %v, want %v", got["review.human.approved"], tt.want)
			}
		})
	}
}

// review.changes_requested is folded to each reviewer's latest decision: a
// REQUEST_CHANGES later superseded by that same reviewer's APPROVED no longer
// counts, which is the "resolved" unhappy path the issue calls out.
func TestReviewChangesRequestedFoldsToLatestDecision(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reviews []github.ReviewReport
		want    bool
	}{
		{"outstanding request", []github.ReviewReport{
			{ID: 1, Login: "alice", State: github.StateChangesRequested, CommitID: "head1"},
		}, true},
		{"resolved by a later approval from the same reviewer", []github.ReviewReport{
			{ID: 1, Login: "alice", State: github.StateChangesRequested, CommitID: "head1"},
			{ID: 2, Login: "alice", State: github.StateApproved, CommitID: "head2"},
		}, false},
		{"a later COMMENTED does not resolve it", []github.ReviewReport{
			{ID: 1, Login: "alice", State: github.StateChangesRequested, CommitID: "head1"},
			{ID: 2, Login: "alice", State: "COMMENTED", CommitID: "head2"},
		}, true},
		{"dismissed request no longer counts", []github.ReviewReport{
			{ID: 1, Login: "alice", State: "DISMISSED", CommitID: "head1"},
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PR(context.Background(), fakeSource{pr: reviewPR(), reviews: tt.reviews},
				"opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			if got["review.changes_requested"] != tt.want {
				t.Errorf("review.changes_requested = %v, want %v", got["review.changes_requested"], tt.want)
			}
		})
	}
}

// review.<team>.* is asserted for every teams.yaml key and every directly
// requested team slug, resolved to a CODEOWNERS-derived membership proxy
// (Evgeny's call, facts.md "review.<team>.approved").
func TestReviewTeamFacts(t *testing.T) {
	const co = "/critical/  @org/security @alice @bob\n"
	teams := config.Teams{"senior_engineer": "@org/security"}

	pr := reviewPR()
	pr.Requested.Teams = []string{"security"}

	for _, tt := range []struct {
		name    string
		reviews []github.ReviewReport
		want    map[string]any
	}{
		{
			name:    "requested, not yet approved",
			reviews: nil,
			want: map[string]any{
				"review.senior_engineer.requested": true,
				"review.senior_engineer.approved":  false,
				"review.senior_engineer.stale":     false,
			},
		},
		{
			name: "approved by a codeowners-listed proxy member at head",
			reviews: []github.ReviewReport{
				{ID: 1, Login: "alice", State: github.StateApproved, CommitID: "head2"},
			},
			want: map[string]any{
				"review.senior_engineer.approved": true,
				"review.senior_engineer.stale":    false,
			},
		},
		{
			name: "approval at a stale sha reports stale, not approved",
			reviews: []github.ReviewReport{
				{ID: 1, Login: "bob", State: github.StateApproved, CommitID: "head1"},
			},
			want: map[string]any{
				"review.senior_engineer.approved": false,
				"review.senior_engineer.stale":    true,
			},
		},
		{
			name: "an approval from someone codeowners never lists alongside the team does not count",
			reviews: []github.ReviewReport{
				{ID: 1, Login: "carol", State: github.StateApproved, CommitID: "head2"},
			},
			want: map[string]any{
				"review.senior_engineer.approved": false,
				"review.senior_engineer.stale":    false,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := fakeSource{pr: pr, files: []string{"critical/x.go"}, reviews: tt.reviews}
			got, err := PR(context.Background(), src, "opentalon", "talooner", 42, config.Checks{}, []byte(co), nil, teams, nil)
			if err != nil {
				t.Fatalf("PR: %v", err)
			}
			for name, want := range tt.want {
				if got[name] != want {
					t.Errorf("%s = %v, want %v", name, got[name], want)
				}
			}
		})
	}
}

// A team requested directly (no teams.yaml entry) falls back to the
// path-derived slug in the repo's own organisation, same as
// assignment.ResolveReviewer.
func TestReviewTeamFactsPathDerivedFallback(t *testing.T) {
	pr := reviewPR()
	pr.Requested.Teams = []string{"security"}

	got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if got["review.security.requested"] != true {
		t.Errorf("review.security.requested = %v, want true", got["review.security.requested"])
	}
}

// The same physical team never gets two fact names: a directly requested slug
// that resolves to a teams.yaml entry's own target is folded into that
// logical name instead of asserting a second, redundant set of facts.
func TestReviewTeamFactsDoNotDuplicateAcrossNames(t *testing.T) {
	teams := config.Teams{"senior_engineer": "@opentalon/security"}
	pr := reviewPR()
	pr.Requested.Teams = []string{"security"} // same team teams.yaml already names

	got, err := PR(context.Background(), fakeSource{pr: pr}, "opentalon", "talooner", 42, config.Checks{}, nil, nil, teams, nil)
	if err != nil {
		t.Fatalf("PR: %v", err)
	}
	if _, ok := got["review.security.requested"]; ok {
		t.Error("review.security.* asserted alongside review.senior_engineer.*, want one name for one team")
	}
	if got["review.senior_engineer.requested"] != true {
		t.Errorf("review.senior_engineer.requested = %v, want true", got["review.senior_engineer.requested"])
	}
}

// A failing review fetch is no-partial-set, the same rule as every other
// extractor call.
func TestReviewFetchFailureReturnsNoPartialSet(t *testing.T) {
	boom := errors.New("boom")
	got, err := PR(context.Background(), fakeSource{pr: samplePR(), reviewErr: boom},
		"opentalon", "talooner", 42, config.Checks{}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("err = nil, want the fetch failure")
	}
	if got != nil {
		t.Errorf("facts = %v, want nil", got)
	}
}

func TestTeamProxyMembers(t *testing.T) {
	rules := parseCodeowners([]byte("/critical/  @org/security @alice @bob\n/docs/  @org/docs\n"))

	members := teamProxyMembers(rules, []string{"critical/x.go"}, "org/security")
	for _, login := range []string{"alice", "bob"} {
		if !members[login] {
			t.Errorf("members[%s] = false, want true", login)
		}
	}
	if members["carol"] {
		t.Error(`members["carol"] = true, want false`)
	}

	// A rule that never lists the team alongside anyone yields no proxy
	// members for it — the documented gap, not a guess.
	none := teamProxyMembers(rules, []string{"docs/x.md"}, "org/security")
	if len(none) != 0 {
		t.Errorf("members = %v, want none: docs/ never lists org/security", none)
	}
}
