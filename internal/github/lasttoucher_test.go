package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// commitsServer answers /repos/o/r/commits, keying its response on the "path"
// query param. A path with no entry in bodies answers an empty array, the same
// shape GitHub returns for a path with no commit history. It also asserts sha
// is forwarded on every call and counts requests, so a cap test can check no
// more than expected fired.
func commitsServer(t *testing.T, bodies map[string]string, wantSHA string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/commits") {
			t.Errorf("path = %s, want /repos/o/r/commits", r.URL.Path)
		}
		if got := r.URL.Query().Get("sha"); got != wantSHA {
			t.Errorf("sha = %q, want %q", got, wantSHA)
		}
		p := r.URL.Query().Get("path")
		body, ok := bodies[p]
		if !ok {
			body = "[]"
		}
		_, _ = fmt.Fprint(w, body)
	}))
	return srv, &calls
}

func TestLastToucherPicksTheMostRecentCommitAcrossPaths(t *testing.T) {
	bodies := map[string]string{
		"a.go": `[{"commit":{"author":{"date":"2026-01-01T00:00:00Z"}},"author":{"login":"alice"}}]`,
		"b.go": `[{"commit":{"author":{"date":"2026-06-01T00:00:00Z"}},"author":{"login":"bob"}}]`,
	}
	srv, _ := commitsServer(t, bodies, "deadbeef")
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

// A commit whose author has no linked GitHub account contributes nothing —
// never guessed from the raw git name or email.
func TestLastToucherSkipsUnlinkedAuthor(t *testing.T) {
	bodies := map[string]string{
		"a.go": `[{"commit":{"author":{"date":"2026-06-01T00:00:00Z"}},"author":null}]`,
		"b.go": `[{"commit":{"author":{"date":"2026-01-01T00:00:00Z"}},"author":{"login":"bob"}}]`,
	}
	srv, _ := commitsServer(t, bodies, "sha1")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	got, err := c.LastToucher(context.Background(), "o", "r", "sha1", []string{"a.go", "b.go"})
	if err != nil {
		t.Fatalf("LastToucher: %v", err)
	}
	if got != "bob" {
		t.Errorf("LastToucher = %q, want bob (a.go's commit has no linked account)", got)
	}
}

// A path with no prior commit — a file this PR adds — resolves "" like any
// other empty answer, not an error.
func TestLastToucherEmptyWhenNoPathHasHistory(t *testing.T) {
	srv, _ := commitsServer(t, nil, "sha1")
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

// The cap protects against a huge PR firing hundreds of sequential calls: only
// the first maxLastToucherPaths changed paths are ever queried.
func TestLastToucherCapsPathsQueried(t *testing.T) {
	paths := make([]string, maxLastToucherPaths+10)
	for i := range paths {
		paths[i] = fmt.Sprintf("f%d.go", i)
	}
	srv, calls := commitsServer(t, nil, "sha1")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.LastToucher(context.Background(), "o", "r", "sha1", paths); err != nil {
		t.Fatalf("LastToucher: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != maxLastToucherPaths {
		t.Errorf("calls = %d, want %d (the cap)", got, maxLastToucherPaths)
	}
}
