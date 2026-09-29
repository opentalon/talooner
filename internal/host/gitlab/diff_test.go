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

func diffsServer(t *testing.T, body string, link ...string) *httptest.Server {
	t.Helper()
	hdr := ""
	if len(link) > 0 {
		hdr = link[0]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests/1/diffs") {
			t.Errorf("path = %s, want /projects/o%%2Fr/merge_requests/1/diffs", r.URL.Path)
		}
		if hdr != "" {
			w.Header().Set("Link", hdr)
		}
		_, _ = fmt.Fprint(w, body)
	}))
}

func TestGitLabDiffConcatenatesPatches(t *testing.T) {
	srv := diffsServer(t, `[{"new_path":"a.go","diff":"@@ -1 +1 @@\n-a\n+b"},{"new_path":"b.go","diff":"@@ -1 +1 @@\n-c"}]`)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	diff, trunc, err := c.Diff(context.Background(), "o", "r", 1, host.DiffMaxBytes)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if trunc {
		t.Error("trunc = true, want false for a tiny diff")
	}
	want := "@@ -1 +1 @@\n-a\n+b\n@@ -1 +1 @@\n-c"
	if diff != want {
		t.Errorf("diff = %q, want %q", diff, want)
	}
}

func TestGitLabDiffSkipsEmptyDiffEntries(t *testing.T) {
	srv := diffsServer(t, `[{"new_path":"img.png","diff":""},{"new_path":"a.go","diff":"@@ -1 +1 @@\n+x"}]`)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	diff, _, err := c.Diff(context.Background(), "o", "r", 1, host.DiffMaxBytes)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "@@ -1 +1 @@\n+x" {
		t.Errorf("diff = %q, want the one textual patch only", diff)
	}
}

func TestGitLabDiffCapBoundaries(t *testing.T) {
	a := "AAAA"
	b := "BBBB"
	for _, tt := range []struct {
		name  string
		cap   int
		trunc bool
	}{
		{"exactly at cap", 9, false},
		{"one byte under", 10, false},
		{"one byte over", 8, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := diffsServer(t, fmt.Sprintf(`[{"new_path":"a","diff":%q},{"new_path":"b","diff":%q}]`, a, b))
			defer srv.Close()

			c, _ := newTestClient(t, srv)
			diff, trunc, err := c.Diff(context.Background(), "o", "r", 1, tt.cap)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if trunc != tt.trunc {
				t.Errorf("trunc = %v, want %v", trunc, tt.trunc)
			}
			if tt.trunc {
				if diff != a {
					t.Errorf("diff = %q, want only %q (second file dropped)", diff, a)
				}
			} else if diff != a+"\n"+b {
				t.Errorf("diff = %q, want %q", diff, a+"\n"+b)
			}
		})
	}
}

func TestGitLabDiffPaginationTruncatesMidStream(t *testing.T) {
	var srv *httptest.Server
	page2 := `{"new_path":"p2","diff":"YYYYYYYYYY"}`
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "page=2") {
			_, _ = fmt.Fprint(w, "["+page2+"]")
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/projects/o%%2Fr/merge_requests/1/diffs?per_page=100&page=2>; rel="next"`, srv.URL))
		_, _ = fmt.Fprint(w, `[{"new_path":"p1","diff":"XXXXXXXXXX"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	diff, trunc, err := c.Diff(context.Background(), "o", "r", 1, 15)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !trunc {
		t.Error("trunc = false, want true: page two overflows")
	}
	if diff != "XXXXXXXXXX" {
		t.Errorf("diff = %q, want only page one (page two dropped)", diff)
	}
}

func TestGitLabDiffFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	if _, _, err := c.Diff(context.Background(), "o", "r", 1, host.DiffMaxBytes); err == nil {
		t.Fatal("Diff: want error, got nil")
	}
}

func TestGitLabDiffRejectsBadArguments(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tt := range []struct {
		number   int
		maxBytes int
	}{
		{0, host.DiffMaxBytes},
		{-1, host.DiffMaxBytes},
		{1, 0},
		{1, -5},
	} {
		if _, _, err := c.Diff(context.Background(), "o", "r", tt.number, tt.maxBytes); err == nil {
			t.Errorf("Diff(%d, %d): want error, got nil", tt.number, tt.maxBytes)
		}
	}
}
