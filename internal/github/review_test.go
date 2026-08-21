package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testMarker = "<!-- talooner:v1:verdict -->"

// reviewServer is the three endpoints a review sync touches: the listing, the
// submit, and the dismissal. It records both writes, so a test can assert what
// was retracted as easily as what was submitted.
type reviewServer struct {
	mu sync.Mutex
	// existing is what the listing returns, i.e. what earlier runs and humans
	// left on the PR.
	existing   []reviewPayload
	submitted  []submittedReview
	dismissed  []string // review ids, in the order they were dismissed
	dismissals []string // the messages that went with them
	// order records every write, so "dismiss before submit" is testable.
	order []string

	listStatus    int
	submitStatus  int
	dismissStatus int
}

type submittedReview struct {
	CommitID string `json:"commit_id"`
	Body     string `json:"body"`
	Event    string `json:"event"`
}

func (s *reviewServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodGet:
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			raw, err := json.Marshal(s.existing)
			if err != nil {
				t.Errorf("encode existing reviews: %v", err)
			}
			_, _ = w.Write(raw)

		case strings.HasSuffix(r.URL.Path, "/dismissals") && r.Method == http.MethodPut:
			if s.dismissStatus != 0 {
				w.WriteHeader(s.dismissStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var body struct {
				Message string `json:"message"`
				Event   string `json:"event"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode dismissal: %v", err)
			}
			if body.Event != "DISMISS" {
				t.Errorf("dismissal event = %q, want DISMISS", body.Event)
			}
			parts := strings.Split(r.URL.Path, "/")
			s.dismissed = append(s.dismissed, parts[len(parts)-2])
			s.dismissals = append(s.dismissals, body.Message)
			s.order = append(s.order, "dismiss")
			_, _ = fmt.Fprint(w, `{}`)

		case strings.HasSuffix(r.URL.Path, "/reviews") && r.Method == http.MethodPost:
			if s.submitStatus != 0 {
				w.WriteHeader(s.submitStatus)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			var got submittedReview
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode review: %v", err)
			}
			s.submitted = append(s.submitted, got)
			s.order = append(s.order, "submit")
			_, _ = fmt.Fprint(w, `{"id":777}`)

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(testToken, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func approval(marker string) Review {
	return Review{
		Marker:         marker,
		Event:          ReviewApprove,
		Body:           "### Talooner approves\n",
		CommitID:       "abc123",
		DismissMessage: "no longer holds",
	}
}

func TestSyncReviewSubmitsWhenNothingIsStanding(t *testing.T) {
	s := &reviewServer{}

	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker))
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 777 {
		t.Errorf("id = %d, want the submitted review's", id)
	}
	if len(s.submitted) != 1 {
		t.Fatalf("reviews submitted = %d, want 1", len(s.submitted))
	}
	got := s.submitted[0]
	if got.Event != ReviewApprove || got.CommitID != "abc123" {
		t.Errorf("submitted %+v, want an APPROVE at abc123", got)
	}
	if !strings.HasPrefix(got.Body, testMarker) {
		t.Errorf("body does not carry the marker: %q", got.Body)
	}
	if len(s.dismissed) != 0 {
		t.Errorf("dismissed %v with nothing standing", s.dismissed)
	}
}

// The verdict did not change, so the review does not either: re-submitting the
// same approval on every push costs every reviewer an email and says nothing.
func TestSyncReviewLeavesAStandingVerdictAlone(t *testing.T) {
	s := &reviewServer{existing: []reviewPayload{
		{ID: 11, Body: "unrelated human review", State: StateApproved},
		{ID: 12, Body: testMarker + "\napproved", State: StateApproved, CommitID: "old"},
	}}

	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker))
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 12 {
		t.Errorf("id = %d, want the review already standing", id)
	}
	if len(s.submitted) != 0 || len(s.dismissed) != 0 {
		t.Errorf("wrote something: submitted %v, dismissed %v", s.submitted, s.dismissed)
	}
}

// approve then block at the next sha: the earlier approval is dismissed, not
// left standing alongside the request for changes. Dismissal comes first — a
// submit that lands and a dismissal that then fails would leave an approval
// standing, which is the permissive one to get wrong.
func TestSyncReviewDismissesTheOppositeVerdictFirst(t *testing.T) {
	s := &reviewServer{existing: []reviewPayload{
		{ID: 12, Body: testMarker + "\napproved", State: StateApproved},
	}}

	rv := approval(testMarker)
	rv.Event = ReviewRequestChanges
	rv.Body = "### Talooner requests changes\n"
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if strings.Join(s.order, ",") != "dismiss,submit" {
		t.Errorf("order = %v, want the dismissal before the submit", s.order)
	}
	if len(s.dismissed) != 1 || s.dismissed[0] != "12" {
		t.Errorf("dismissed = %v, want the standing approval", s.dismissed)
	}
	if s.dismissals[0] != rv.DismissMessage {
		t.Errorf("dismissal message = %q", s.dismissals[0])
	}
}

// No verdict this run: whatever the last one left is retracted, and nothing is
// submitted in its place.
func TestSyncReviewWithNoEventRetracts(t *testing.T) {
	s := &reviewServer{existing: []reviewPayload{
		{ID: 12, Body: testMarker + "\napproved", State: StateApproved},
	}}

	rv := approval(testMarker)
	rv.Event, rv.Body, rv.CommitID = "", "", ""
	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv)
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want none standing", id)
	}
	if len(s.dismissed) != 1 || len(s.submitted) != 0 {
		t.Errorf("dismissed %v, submitted %v", s.dismissed, s.submitted)
	}
}

// A human dismissed it already, or Talooner never wrote one: retraction is a
// no-op rather than a 422 against a review that is not standing.
func TestSyncReviewRetractingNothingIsNotAnError(t *testing.T) {
	s := &reviewServer{existing: []reviewPayload{
		{ID: 12, Body: testMarker + "\napproved", State: "DISMISSED"},
		{ID: 13, Body: testMarker + "\nsome note", State: "COMMENTED"},
		{ID: 14, Body: "a human's approval", State: StateApproved},
	}}

	rv := approval(testMarker)
	rv.Event, rv.Body, rv.CommitID = "", "", ""
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if len(s.dismissed) != 0 {
		t.Errorf("dismissed %v; none of those is a standing talooner verdict", s.dismissed)
	}
}

// A botched earlier run left two. One is kept and the rest retracted, so the
// state converges instead of growing by one review per run.
func TestSyncReviewKeepsOneAndDismissesTheRest(t *testing.T) {
	s := &reviewServer{existing: []reviewPayload{
		{ID: 12, Body: testMarker + "\napproved", State: StateApproved},
		{ID: 13, Body: testMarker + "\napproved again", State: StateApproved},
	}}

	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker))
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 12 {
		t.Errorf("id = %d, want the oldest kept", id)
	}
	if len(s.dismissed) != 1 || s.dismissed[0] != "13" {
		t.Errorf("dismissed = %v, want the duplicate", s.dismissed)
	}
	if len(s.submitted) != 0 {
		t.Errorf("submitted %v; one was already standing", s.submitted)
	}
}

// A listing that fails takes the call down. Falling through to a submit is how
// a PR ends up with an undismissed approval next to a request for changes.
func TestSyncReviewFailsWhenTheListingFails(t *testing.T) {
	s := &reviewServer{listStatus: http.StatusInternalServerError}

	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker)); err == nil {
		t.Fatal("SyncReview succeeded with a broken listing")
	} else if !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want a server error", err)
	}
	if len(s.submitted) != 0 {
		t.Errorf("submitted %v after the listing failed", s.submitted)
	}
}

// The dismissal is what makes the retraction real, so a failed one fails the
// run rather than being papered over with a fresh review.
func TestSyncReviewFailsWhenTheDismissalFails(t *testing.T) {
	s := &reviewServer{
		existing:      []reviewPayload{{ID: 12, Body: testMarker, State: StateApproved}},
		dismissStatus: http.StatusForbidden,
	}

	rv := approval(testMarker)
	rv.Event = ReviewRequestChanges
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err == nil {
		t.Fatal("SyncReview succeeded with a broken dismissal")
	}
	if len(s.submitted) != 0 {
		t.Errorf("submitted %v while the old verdict was still standing", s.submitted)
	}
}

// A review dismissed between the listing and the dismissal is the outcome the
// call wanted, so a 404 there is not a failure.
func TestSyncReviewToleratesAReviewThatDisappeared(t *testing.T) {
	s := &reviewServer{
		existing:      []reviewPayload{{ID: 12, Body: testMarker, State: StateApproved}},
		dismissStatus: http.StatusNotFound,
	}

	rv := approval(testMarker)
	rv.Event, rv.Body, rv.CommitID = "", "", ""
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
}

func TestSyncReviewRejectsUnperformableReviews(t *testing.T) {
	tests := map[string]func(*Review){
		"no marker":          func(rv *Review) { rv.Marker = "" },
		"marker spans lines": func(rv *Review) { rv.Marker = "<!--\ntalooner -->" },
		"no dismiss message": func(rv *Review) { rv.DismissMessage = "" },
		"unknown event":      func(rv *Review) { rv.Event = "COMMENT" },
		"no body":            func(rv *Review) { rv.Body = "  " },
		"no commit id":       func(rv *Review) { rv.CommitID = "" },
		"body forges the marker": func(rv *Review) {
			rv.Body = "nice PR " + testMarker
		},
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			s := &reviewServer{}
			rv := approval(testMarker)
			break_(&rv)
			if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err == nil {
				t.Fatalf("SyncReview accepted %s", name)
			}
			if len(s.submitted) != 0 || len(s.dismissed) != 0 {
				t.Errorf("wrote something for %s", name)
			}
		})
	}
}

func TestSyncReviewRejectsANonPositivePullRequest(t *testing.T) {
	s := &reviewServer{}
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 0, approval(testMarker)); err == nil {
		t.Fatal("SyncReview accepted pull request 0")
	}
}

// PullRequestReviews reads the same listing SyncReview does, unfiltered by
// marker: it is the whole history a fact extractor folds to current state.
func TestPullRequestReviewsReturnsEveryEntry(t *testing.T) {
	s := &reviewServer{existing: []reviewPayload{
		{ID: 1, State: StateApproved, CommitID: "abc", User: &reviewUser{Login: "alice", Type: "User"}},
		{ID: 2, State: "COMMENTED", User: &reviewUser{Login: "dependabot", Type: "Bot"}},
	}}

	got, err := s.client(t).PullRequestReviews(t.Context(), "opentalon", "talooner", 42)
	if err != nil {
		t.Fatalf("PullRequestReviews: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d reviews, want 2", len(got))
	}
	if got[0].Login != "alice" || got[0].Bot || got[0].State != StateApproved || got[0].CommitID != "abc" {
		t.Errorf("got[0] = %+v, want alice's approval at abc", got[0])
	}
	if got[1].Login != "dependabot" || !got[1].Bot {
		t.Errorf("got[1] = %+v, want dependabot flagged as a bot", got[1])
	}
}

func TestPullRequestReviewsFailsWhenTheListingFails(t *testing.T) {
	s := &reviewServer{listStatus: http.StatusInternalServerError}
	if _, err := s.client(t).PullRequestReviews(t.Context(), "opentalon", "talooner", 42); err == nil {
		t.Fatal("PullRequestReviews succeeded with a broken listing")
	}
}

func TestPullRequestReviewsRejectsANonPositivePullRequest(t *testing.T) {
	s := &reviewServer{}
	if _, err := s.client(t).PullRequestReviews(t.Context(), "opentalon", "talooner", 0); err == nil {
		t.Fatal("PullRequestReviews accepted pull request 0")
	}
}
