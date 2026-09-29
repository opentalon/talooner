package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// toucherServer answers /projects/o/r/repository/commits, keying its
// response on the "path" query param, and /users, keying on "search" (the
// email lookup). A path with no entry answers an empty array, GitLab's shape
// for a path with no commit history.
func toucherServer(t *testing.T, commits map[string]string, users map[string]string, wantRef string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/users") {
			email := r.URL.Query().Get("search")
			username, ok := users[email]
			if !ok {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			_, _ = fmt.Fprintf(w, `[{"username":%s}]`, jsonString(username))
			return
		}
		atomic.AddInt32(&calls, 1)
		if !strings.HasSuffix(r.URL.Path, "/repository/commits") {
			t.Errorf("path = %s, want .../repository/commits", r.URL.Path)
		}
		if got := r.URL.Query().Get("ref_name"); got != wantRef {
			t.Errorf("ref_name = %q, want %q", got, wantRef)
		}
		p := r.URL.Query().Get("path")
		body, ok := commits[p]
		if !ok {
			body = "[]"
		}
		_, _ = fmt.Fprint(w, body)
	}))
	return srv, &calls
}

func TestGitLabLastToucherPicksTheMostRecentCommitAcrossPaths(t *testing.T) {
	commits := map[string]string{
		"a.go": `[{"author_email":"alice@example.com","authored_date":"2026-01-01T00:00:00Z"}]`,
		"b.go": `[{"author_email":"bob@example.com","authored_date":"2026-06-01T00:00:00Z"}]`,
	}
	users := map[string]string{"alice@example.com": "alice", "bob@example.com": "bob"}
	srv, _ := toucherServer(t, commits, users, "deadbeef")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	got, err := c.LastToucher(context.Background(), "o", "r", "deadbeef", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("LastToucher: %v", err)
	}
	if got != "bob" {
		t.Errorf("LastToucher = %q, want bob (the later commit)", got)
	}
}

func TestGitLabLastToucherSkipsUnlinkedAuthor(t *testing.T) {
	commits := map[string]string{
		"a.go": `[{"author_email":"ghost@example.com","authored_date":"2026-06-01T00:00:00Z"}]`,
		"b.go": `[{"author_email":"bob@example.com","authored_date":"2026-01-01T00:00:00Z"}]`,
	}
	users := map[string]string{"bob@example.com": "bob"}
	srv, _ := toucherServer(t, commits, users, "sha1")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	got, err := c.LastToucher(context.Background(), "o", "r", "sha1", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("LastToucher: %v", err)
	}
	if got != "bob" {
		t.Errorf("LastToucher = %q, want bob (a.go's most recent author has no linked account)", got)
	}
}

func TestGitLabLastToucherEmptyWhenNoPathHasHistory(t *testing.T) {
	srv, _ := toucherServer(t, nil, nil, "sha1")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	got, err := c.LastToucher(context.Background(), "o", "r", "sha1", []string{"new.go"})
	if err != nil {
		t.Fatalf("LastToucher: %v", err)
	}
	if got != "" {
		t.Errorf("LastToucher = %q, want empty", got)
	}
}

func TestGitLabLastToucherCapsPathsQueried(t *testing.T) {
	paths := make([]string, maxLastToucherPaths+10)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%d.go", i)
	}
	srv, calls := toucherServer(t, nil, nil, "sha1")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.LastToucher(context.Background(), "o", "r", "sha1", paths); err != nil {
		t.Fatalf("LastToucher: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != maxLastToucherPaths {
		t.Errorf("calls = %d, want %d (the cap)", got, maxLastToucherPaths)
	}
}
