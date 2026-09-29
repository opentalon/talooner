package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

func TestStatusOfMapsGitLabVocabularyToNeutral(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"pending", host.StatusPending},
		{"created", host.StatusPending},
		{"running", host.StatusPending},
		{"success", host.ConclusionSuccess},
		{"failed", host.ConclusionFailure},
		{"canceled", host.ConclusionFailure},
		{"skipped", "skipped"},
	} {
		if got := statusOf(tt.in); got != tt.want {
			t.Errorf("statusOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGitLabCommitChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repository/commits/abc123/statuses") {
			t.Errorf("path = %s, want .../repository/commits/abc123/statuses", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `[{"name":"build","status":"running"},{"name":"lint","status":"success"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	checks, err := c.CommitChecks(context.Background(), "o", "r", "abc123")
	if err != nil {
		t.Fatalf("CommitChecks: %v", err)
	}
	if len(checks.Runs) != 0 {
		t.Errorf("Runs = %+v, want none: GitLab Core has no check-run equivalent", checks.Runs)
	}
	if len(checks.Statuses) != 2 || checks.Statuses[0].Context != "build" || checks.Statuses[0].State != host.StatusPending {
		t.Errorf("Statuses = %+v, want build pending first", checks.Statuses)
	}
	if checks.Statuses[1].State != host.ConclusionSuccess {
		t.Errorf("Statuses[1].State = %q, want success", checks.Statuses[1].State)
	}
	if !checks.Pending() {
		t.Error("Pending() = false, want true: a running status is pending")
	}
}

func TestGitLabCommitChecksEmptyCI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	checks, err := c.CommitChecks(context.Background(), "o", "r", "abc123")
	if err != nil {
		t.Fatalf("CommitChecks: %v", err)
	}
	if checks.Pending() {
		t.Error("Pending() = true, want false: an empty CI is settled, asserted")
	}
	if len(checks.Statuses) != 0 {
		t.Errorf("Statuses = %+v, want empty", checks.Statuses)
	}
}

func TestGitLabCommitChecksPagination(t *testing.T) {
	var srv *httptest.Server
	var hits int
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/projects/o%%2Fr/repository/commits/abc123/statuses?per_page=100&page=2>; rel="next"`, srv.URL))
			_, _ = fmt.Fprint(w, `[{"name":"a","status":"success"}]`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"name":"b","status":"pending"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	checks, err := c.CommitChecks(context.Background(), "o", "r", "abc123")
	if err != nil {
		t.Fatalf("CommitChecks: %v", err)
	}
	if hits != 2 {
		t.Errorf("endpoint hit %d times, want 2 across the pagination", hits)
	}
	if len(checks.Statuses) != 2 || checks.Statuses[0].Context != "a" || checks.Statuses[1].Context != "b" {
		t.Errorf("Statuses = %+v, want both pages' statuses", checks.Statuses)
	}
	if !checks.Pending() {
		t.Error("Pending() = false, want true: page two carried a pending status")
	}
}

func TestGitLabCommitChecksFailsOnPageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	if _, err := c.CommitChecks(context.Background(), "o", "r", "abc123"); err == nil {
		t.Fatal("CommitChecks: want error, got nil")
	}
}

func TestGitLabCommitChecksRejectsEmptySHA(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.CommitChecks(context.Background(), "o", "r", ""); err == nil {
		t.Error("CommitChecks with empty sha: want error, got nil")
	}
}
