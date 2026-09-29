package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// peopleServer answers the assignee and review-request endpoints, recording
// what reached them.
type peopleServer struct {
	assignees []string
	users     []string
	teams     []string

	methods []string
	bodies  []string
	status  int
}

func (s *peopleServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		s.methods = append(s.methods, r.Method)
		s.bodies = append(s.bodies, string(raw))

		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = fmt.Fprint(w, `{"message":"Reviews may only be requested from collaborators."}`)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/assignees"):
			_, _ = fmt.Fprintf(w, `{"assignees":%s}`, logins(s.assignees))
		default:
			_, _ = fmt.Fprintf(w, `{"requested_reviewers":%s,"requested_teams":%s}`, logins(s.users), slugs(s.teams))
		}
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv)
	return c
}

func logins(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf(`{"login":%q}`, n))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func slugs(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf(`{"slug":%q}`, n))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// The response, not the status code, is what says an assignment happened:
// GitHub answers 201 and leaves out a login it will not assign.
func TestAddAssigneesReportsWhatGitHubRecorded(t *testing.T) {
	s := &peopleServer{assignees: []string{"alice"}}

	got, err := s.client(t).AddAssignees(t.Context(), "opentalon", "talooner", 42, []string{"alice", "stranger"})
	if err != nil {
		t.Fatalf("AddAssignees: %v", err)
	}
	if len(got) != 1 || got[0] != "alice" {
		t.Errorf("recorded = %v, want only alice: stranger was dropped by github", got)
	}
	if s.methods[0] != http.MethodPost {
		t.Errorf("method = %s, want POST", s.methods[0])
	}
	var sent struct {
		Assignees []string `json:"assignees"`
	}
	if err := json.Unmarshal([]byte(s.bodies[0]), &sent); err != nil {
		t.Fatalf("decode what was sent: %v", err)
	}
	if len(sent.Assignees) != 2 {
		t.Errorf("sent %v, want both logins in one call", sent.Assignees)
	}
}

func TestRemoveAssigneesUsesDelete(t *testing.T) {
	s := &peopleServer{}

	if _, err := s.client(t).RemoveAssignees(t.Context(), "opentalon", "talooner", 42, []string{"alice"}); err != nil {
		t.Fatalf("RemoveAssignees: %v", err)
	}
	if s.methods[0] != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", s.methods[0])
	}
}

func TestReviewRequestsRoundTrip(t *testing.T) {
	s := &peopleServer{users: []string{"bob"}, teams: []string{"security"}}

	got, err := s.client(t).RequestReviewers(t.Context(), "opentalon", "talooner", 42, []string{"bob"}, []string{"security"})
	if err != nil {
		t.Fatalf("RequestReviewers: %v", err)
	}
	if len(got.Users) != 1 || got.Users[0] != "bob" || len(got.Teams) != 1 || got.Teams[0] != "security" {
		t.Errorf("standing = %+v, want bob and security", got)
	}
	var sent struct {
		Reviewers []string `json:"reviewers"`
		Teams     []string `json:"team_reviewers"`
	}
	if err := json.Unmarshal([]byte(s.bodies[0]), &sent); err != nil {
		t.Fatalf("decode what was sent: %v", err)
	}
	if len(sent.Reviewers) != 1 || len(sent.Teams) != 1 {
		t.Errorf("sent %+v, want users and teams in their own fields", sent)
	}
}

// GitHub rejects the whole call when one reviewer cannot be asked, so there is
// no half-success to detect here — the error is the report, and it has to name
// who was being asked.
func TestARefusedReviewRequestIsAnError(t *testing.T) {
	s := &peopleServer{status: http.StatusUnprocessableEntity}

	_, err := s.client(t).RequestReviewers(t.Context(), "opentalon", "talooner", 42, []string{"stranger"}, nil)
	if err == nil {
		t.Fatal("RequestReviewers = nil, want the 422")
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("err = %v, want it to name the reviewer", err)
	}
}

func TestPeopleWritesRefuseAnEmptyList(t *testing.T) {
	c := (&peopleServer{}).client(t)

	if _, err := c.AddAssignees(t.Context(), "opentalon", "talooner", 42, nil); err == nil {
		t.Error("AddAssignees = nil, want an empty list refused rather than sent")
	}
	if _, err := c.RequestReviewers(t.Context(), "opentalon", "talooner", 42, nil, nil); err == nil {
		t.Error("RequestReviewers = nil, want an empty list refused rather than sent")
	}
	if _, err := c.AddAssignees(t.Context(), "opentalon", "talooner", 0, []string{"alice"}); err == nil {
		t.Error("AddAssignees = nil, want a zero pull request number refused")
	}
}

// The pull request fetch carries the state assignment reconciles against, so it
// has to
// decode it: a missing assignee list would read as "a human added nobody".
func TestPullRequestCarriesAssigneesAndRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{
			"number": 42,
			"head": {"sha": "abc123", "ref": "feat/x", "repo": {"full_name": "o/r"}},
			"base": {"sha": "def456", "ref": "master", "repo": {"full_name": "o/r"}},
			"assignees": [{"login": "alice"}],
			"requested_reviewers": [{"login": "bob"}],
			"requested_teams": [{"slug": "security"}]
		}`)
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv)

	pr, err := c.PullRequest(t.Context(), "o", "r", 42)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if len(pr.Assignees) != 1 || pr.Assignees[0] != "alice" {
		t.Errorf("assignees = %v, want alice", pr.Assignees)
	}
	if len(pr.Requested.Users) != 1 || pr.Requested.Users[0] != "bob" {
		t.Errorf("requested users = %v, want bob", pr.Requested.Users)
	}
	if len(pr.Requested.Teams) != 1 || pr.Requested.Teams[0] != "security" {
		t.Errorf("requested teams = %v, want security", pr.Requested.Teams)
	}
}

// CommentBody is how the ledger is read back. A topic with no comment is an
// empty state rather than an error, and that difference is load-bearing: an
// error means "own nothing", and so does an empty body, but only one of them is
// worth a warning in the log.
func TestCommentBody(t *testing.T) {
	const marker = "<!-- talooner:v1:state -->"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"id":5,"body":"other"},{"id":9,"body":%q},{"id":11,"body":%q}]`,
			marker+"\nfirst", marker+"\nsecond")
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv)

	got, err := c.CommentBody(t.Context(), "o", "r", 42, marker)
	if err != nil {
		t.Fatalf("CommentBody: %v", err)
	}
	if !strings.Contains(got, "first") {
		t.Errorf("body = %q, want the oldest comment carrying the marker", got)
	}

	missing, err := c.CommentBody(t.Context(), "o", "r", 42, "<!-- talooner:v1:nothing -->")
	if err != nil || missing != "" {
		t.Errorf("CommentBody for an absent topic = %q, %v, want an empty body and no error", missing, err)
	}
	if _, err := c.CommentBody(t.Context(), "o", "r", 42, ""); err == nil {
		t.Error("CommentBody with no marker = nil, want an error: that matches every comment")
	}
}
