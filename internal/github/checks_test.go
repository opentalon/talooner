package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChecksPending(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    Checks
		want bool
	}{
		{"queued check run", Checks{Runs: []CheckRunReport{{Status: "queued"}}}, true},
		{"in_progress check run", Checks{Runs: []CheckRunReport{{Status: "in_progress"}}}, true},
		{"completed check run", Checks{Runs: []CheckRunReport{{Status: "completed", Conclusion: "success"}}}, false},
		{"pending commit status", Checks{Statuses: []CommitStatus{{State: "pending"}}}, true},
		{"success commit status", Checks{Statuses: []CommitStatus{{State: "success"}}}, false},
		{"nothing at all", Checks{}, false},
		{"queued run plus settled status", Checks{Runs: []CheckRunReport{{Status: "queued"}}, Statuses: []CommitStatus{{State: "success"}}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.Pending(); got != tt.want {
				t.Errorf("Pending() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommitChecks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = fmt.Fprint(w, `{
				"total_count": 2,
				"check_runs": [
					{"name": "ci/test", "status": "in_progress", "conclusion": null},
					{"name": "ci/lint", "status": "completed", "conclusion": "success"}
				]
			}`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = fmt.Fprint(w, `{
				"state": "success",
				"total_count": 1,
				"statuses": [{"context": "buildkite/rails", "state": "success"}]
			}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	checks, err := c.CommitChecks(context.Background(), "o", "r", "abc123")
	if err != nil {
		t.Fatalf("CommitChecks: %v", err)
	}
	if len(checks.Runs) != 2 || checks.Runs[0].Name != "ci/test" || checks.Runs[0].Status != "in_progress" {
		t.Errorf("runs = %+v, want the two check runs with statuses", checks.Runs)
	}
	if checks.Runs[1].Conclusion != "success" {
		t.Errorf("conclusion = %q, want success", checks.Runs[1].Conclusion)
	}
	if len(checks.Statuses) != 1 || checks.Statuses[0].Context != "buildkite/rails" {
		t.Errorf("statuses = %+v, want the one status", checks.Statuses)
	}
	if !checks.Pending() {
		t.Error("Pending() = false, want true: an in_progress check run is pending")
	}
}

func TestCommitChecksEmptyCI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = fmt.Fprint(w, `{"state":"success","total_count":0,"statuses":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
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
	if len(checks.Runs) != 0 || len(checks.Statuses) != 0 {
		t.Errorf("expected empty checks, got %+v", checks)
	}
}

func TestCommitChecksPagination(t *testing.T) {
	var srv *httptest.Server
	var hits int
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = fmt.Fprint(w, `{"state":"success","total_count":0,"statuses":[]}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/check-runs") {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		hits++
		if hits == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/commits/abc123/check-runs?per_page=100&page=2>; rel="next"`, srv.URL))
			_, _ = fmt.Fprint(w, `{"total_count":2,"check_runs":[{"name":"a","status":"completed"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"total_count":2,"check_runs":[{"name":"b","status":"queued"}]}`)
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
	if len(checks.Runs) != 2 || checks.Runs[0].Name != "a" || checks.Runs[1].Name != "b" {
		t.Errorf("runs = %+v, want both pages' runs", checks.Runs)
	}
	if !checks.Pending() {
		t.Error("Pending() = false, want true: page two carried a queued run")
	}
}

func TestCommitChecksFailsOnPageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	if _, err := c.CommitChecks(context.Background(), "o", "r", "abc123"); err == nil {
		t.Fatal("CommitChecks: want error, got nil")
	}
}

func TestCommitChecksRejectsEmptySHA(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.CommitChecks(context.Background(), "o", "r", ""); err == nil {
		t.Error("CommitChecks with empty sha: want error, got nil")
	}
}
