package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

const testMarker = "<!-- talooner:v1:verdict -->"

// reviewServer is the endpoints a review sync touches: the discussion
// listing, discussion creation, discussion resolution, reply notes, and the
// approval read/approve/unapprove trio. It records every write so a test can
// assert both what was posted and the order writes happened in.
type reviewServer struct {
	mu sync.Mutex

	existing []discussionPayload
	approved bool

	created   []string // bodies of posted discussions
	replied   map[string][]string
	resolved  []resolveCall
	approvals []string // "approve" or "unapprove", in order
	order     []string

	listStatus     int
	createStatus   int
	replyStatus    int
	resolveStatus  int
	approvalStatus int
	approveStatus  int
}

type resolveCall struct {
	discussionID string
	resolved     bool
}

func (s *reviewServer) client(t *testing.T) *Client {
	t.Helper()
	if s.replied == nil {
		s.replied = map[string][]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/discussions") && r.Method == http.MethodGet:
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			raw, err := json.Marshal(s.existing)
			if err != nil {
				t.Errorf("encode existing discussions: %v", err)
			}
			_, _ = w.Write(raw)

		case strings.HasSuffix(path, "/discussions") && r.Method == http.MethodPost:
			if s.createStatus != 0 {
				w.WriteHeader(s.createStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode discussion: %v", err)
			}
			s.created = append(s.created, body.Body)
			s.order = append(s.order, "create")
			_, _ = fmt.Fprint(w, `{"id":"disc-new","notes":[{"id":777,"body":`+jsonString(body.Body)+`}]}`)

		case strings.HasSuffix(path, "/notes") && r.Method == http.MethodPost:
			if s.replyStatus != 0 {
				w.WriteHeader(s.replyStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode reply: %v", err)
			}
			parts := strings.Split(strings.TrimSuffix(path, "/notes"), "/")
			id := parts[len(parts)-1]
			s.replied[id] = append(s.replied[id], body.Body)
			s.order = append(s.order, "reply:"+id)
			_, _ = fmt.Fprint(w, `{"id":999}`)

		case strings.Contains(path, "/discussions/") && r.Method == http.MethodPut:
			if s.resolveStatus != 0 {
				w.WriteHeader(s.resolveStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			s.resolved = append(s.resolved, resolveCall{discussionID: id, resolved: r.URL.Query().Get("resolved") == "true"})
			s.order = append(s.order, "resolve:"+id)
			_, _ = fmt.Fprint(w, `{}`)

		case strings.HasSuffix(path, "/approvals") && r.Method == http.MethodGet:
			if s.approvalStatus != 0 {
				w.WriteHeader(s.approvalStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"user_has_approved":%v}`, s.approved)

		case strings.HasSuffix(path, "/approve") && r.Method == http.MethodPost:
			if s.approveStatus != 0 {
				w.WriteHeader(s.approveStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			s.approvals = append(s.approvals, "approve")
			s.order = append(s.order, "approve")
			_, _ = fmt.Fprint(w, `{}`)

		case strings.HasSuffix(path, "/unapprove") && r.Method == http.MethodPost:
			if s.approveStatus != 0 {
				w.WriteHeader(s.approveStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			s.approvals = append(s.approvals, "unapprove")
			s.order = append(s.order, "unapprove")
			_, _ = fmt.Fprint(w, `{}`)

		default:
			t.Errorf("unexpected %s %s", r.Method, path)
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

func jsonString(s string) string {
	raw, _ := json.Marshal(s) //nolint:errcheck
	return string(raw)
}

func approval(marker string) host.Review {
	return host.Review{
		Marker:         marker,
		Event:          host.ReviewApprove,
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
		t.Errorf("id = %d, want the created note's", id)
	}
	if len(s.created) != 1 || !strings.HasPrefix(s.created[0], testMarker) {
		t.Errorf("created = %v, want one discussion carrying the marker", s.created)
	}
	if len(s.resolved) != 1 || s.resolved[0] != (resolveCall{"disc-new", true}) {
		t.Errorf("resolved = %v, want the fresh approval resolved", s.resolved)
	}
	if len(s.approvals) != 1 || s.approvals[0] != "approve" {
		t.Errorf("approvals = %v, want one approve call", s.approvals)
	}
}

func TestSyncReviewLeavesAStandingVerdictAlone(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 12, Body: testMarker + "\napproved", Resolvable: true, Resolved: true}}},
		},
		approved: true,
	}

	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker))
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 12 {
		t.Errorf("id = %d, want the note already standing", id)
	}
	if len(s.created) != 0 || len(s.resolved) != 0 || len(s.approvals) != 0 {
		t.Errorf("wrote something: created %v, resolved %v, approvals %v", s.created, s.resolved, s.approvals)
	}
}

func TestSyncReviewDismissesTheOppositeVerdictFirst(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 12, Body: testMarker + "\napproved", Resolvable: true, Resolved: true}}},
		},
		approved: true,
	}

	rv := approval(testMarker)
	rv.Event = host.ReviewRequestChanges
	rv.Body = "### Talooner requests changes\n"
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err != nil {
		t.Fatalf("SyncReview: %v", err)
	}

	order := strings.Join(s.order, ",")
	if order != "reply:d1,resolve:d1,unapprove,create" {
		t.Errorf("order = %s, want the dismissal and unapprove before the create", order)
	}
	if len(s.replied["d1"]) != 1 || s.replied["d1"][0] != rv.DismissMessage {
		t.Errorf("replied[d1] = %v, want the dismiss message", s.replied["d1"])
	}
	if len(s.created) != 1 || !strings.HasPrefix(s.created[0], testMarker) {
		t.Errorf("created = %v, want the request-changes thread", s.created)
	}
}

func TestSyncReviewWithNoEventRetracts(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 12, Body: testMarker + "\nblocked", Resolvable: true, Resolved: false}}},
		},
		approved: true,
	}

	rv := approval(testMarker)
	rv.Event, rv.Body, rv.CommitID = "", "", ""
	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv)
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want none standing", id)
	}
	if len(s.replied["d1"]) != 1 || len(s.resolved) != 1 {
		t.Errorf("replied %v, resolved %v, want the thread dismissed", s.replied, s.resolved)
	}
	if len(s.approvals) != 1 || s.approvals[0] != "unapprove" {
		t.Errorf("approvals = %v, want one unapprove call", s.approvals)
	}
	if len(s.created) != 0 {
		t.Errorf("created %v, want nothing new", s.created)
	}
}

func TestSyncReviewRetractingNothingIsNotAnError(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 13, Body: "a human's comment", Resolvable: true, Resolved: false}}},
		},
	}

	rv := approval(testMarker)
	rv.Event, rv.Body, rv.CommitID = "", "", ""
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if len(s.replied) != 0 || len(s.resolved) != 0 {
		t.Errorf("replied %v, resolved %v; none of those is a standing talooner verdict", s.replied, s.resolved)
	}
	if len(s.approvals) != 0 {
		t.Errorf("approvals = %v, want none, was never approved", s.approvals)
	}
}

func TestSyncReviewKeepsOneAndResolvesTheRest(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 12, Body: testMarker + "\napproved", Resolvable: true, Resolved: true}}},
			{ID: "d2", Notes: []discussionNote{{ID: 13, Body: testMarker + "\napproved again", Resolvable: true, Resolved: true}}},
		},
		approved: true,
	}

	id, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker))
	if err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
	if id != 12 {
		t.Errorf("id = %d, want the first kept", id)
	}
	if len(s.replied["d2"]) != 1 {
		t.Errorf("replied[d2] = %v, want the duplicate dismissed", s.replied["d2"])
	}
	if len(s.created) != 0 {
		t.Errorf("created %v; one was already standing", s.created)
	}
}

func TestSyncReviewFailsWhenTheListingFails(t *testing.T) {
	s := &reviewServer{listStatus: http.StatusInternalServerError}

	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker)); err == nil {
		t.Fatal("SyncReview succeeded with a broken listing")
	} else if !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want a server error", err)
	}
	if len(s.created) != 0 {
		t.Errorf("created %v after the listing failed", s.created)
	}
}

func TestSyncReviewApprovalFailureNamesThePermissionCause(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := &reviewServer{approveStatus: status}

			_, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, approval(testMarker))
			if err == nil {
				t.Fatal("SyncReview succeeded with a broken approve call")
			}
			if !errors.Is(err, ErrReviewPermission) {
				t.Errorf("err = %v, want it to wrap ErrReviewPermission", err)
			}
			if !strings.Contains(err.Error(), "TAL-E-REVIEW-PERM") {
				t.Errorf("err = %v, want the TAL-E-REVIEW-PERM code in it", err)
			}
		})
	}
}

func TestSyncReviewFailsWhenTheDismissalFails(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 12, Body: testMarker, Resolvable: true, Resolved: true}}},
		},
		approved:    true,
		replyStatus: http.StatusForbidden,
	}

	rv := approval(testMarker)
	rv.Event = host.ReviewRequestChanges
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err == nil {
		t.Fatal("SyncReview succeeded with a broken dismissal")
	}
	if len(s.created) != 0 {
		t.Errorf("created %v while the old verdict was still standing", s.created)
	}
}

func TestSyncReviewToleratesAThreadThatDisappeared(t *testing.T) {
	s := &reviewServer{
		existing: []discussionPayload{
			{ID: "d1", Notes: []discussionNote{{ID: 12, Body: testMarker, Resolvable: true, Resolved: true}}},
		},
		approved:    true,
		replyStatus: http.StatusNotFound,
	}

	rv := approval(testMarker)
	rv.Event, rv.Body, rv.CommitID = "", "", ""
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err != nil {
		t.Fatalf("SyncReview: %v", err)
	}
}

func TestSyncReviewRejectsUnperformableReviews(t *testing.T) {
	tests := map[string]func(*host.Review){
		"no marker":          func(rv *host.Review) { rv.Marker = "" },
		"marker spans lines": func(rv *host.Review) { rv.Marker = "<!--\ntalooner -->" },
		"no dismiss message": func(rv *host.Review) { rv.DismissMessage = "" },
		"unknown event":      func(rv *host.Review) { rv.Event = "COMMENT" },
		"no body":            func(rv *host.Review) { rv.Body = "  " },
		"no commit id":       func(rv *host.Review) { rv.CommitID = "" },
		"body forges the marker": func(rv *host.Review) {
			rv.Body = "nice MR " + testMarker
		},
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			s := &reviewServer{}
			rv := approval(testMarker)
			breakIt(&rv)
			if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 42, rv); err == nil {
				t.Fatalf("SyncReview accepted %s", name)
			}
			if len(s.created) != 0 || len(s.approvals) != 0 {
				t.Errorf("wrote something for %s", name)
			}
		})
	}
}

func TestSyncReviewRejectsANonPositiveMergeRequest(t *testing.T) {
	s := &reviewServer{}
	if _, err := s.client(t).SyncReview(t.Context(), "opentalon", "talooner", 0, approval(testMarker)); err == nil {
		t.Fatal("SyncReview accepted merge request 0")
	}
}

var _ host.Submitter = (*Client)(nil)
