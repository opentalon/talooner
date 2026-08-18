package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// filesServer returns the given JSON file entries for the /files endpoint and,
// when set, a Link header pointing at a second page on the same server.
func filesServer(t *testing.T, body string, link ...string) *httptest.Server {
	t.Helper()
	hdr := ""
	if len(link) > 0 {
		hdr = link[0]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls/1/files") {
			t.Errorf("path = %s, want /repos/o/r/pulls/1/files", r.URL.Path)
		}
		if hdr != "" {
			w.Header().Set("Link", hdr)
		}
		_, _ = fmt.Fprint(w, body)
	}))
}

func TestDiffConcatenatesPatches(t *testing.T) {
	srv := filesServer(t, `[{"filename":"a.go","patch":"@@ -1 +1 @@\n-a\n+b"},{"filename":"b.go","patch":"@@ -1 +1 @@\n-c"}]`)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	diff, trunc, err := c.Diff(context.Background(), "o", "r", 1, DiffMaxBytes)
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

// Binary files carry a null patch and contribute nothing textual; skipping them
// must not leave a gap or a stray separator.
func TestDiffSkipsBinaryFiles(t *testing.T) {
	srv := filesServer(t, `[{"filename":"logo.png","patch":null},{"filename":"a.go","patch":"@@ -1 +1 @@\n+x"}]`)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	diff, _, err := c.Diff(context.Background(), "o", "r", 1, DiffMaxBytes)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff != "@@ -1 +1 @@\n+x" {
		t.Errorf("diff = %q, want the one textual patch only", diff)
	}
}

// The cap is file-granular. Exactly at the cap is not truncated; one byte over is
// (the overflowing file is dropped); one byte under is included whole.
func TestDiffCapBoundaries(t *testing.T) {
	a := "AAAA" // 4
	b := "BBBB" // 4; with the "\n" separator the two together are 9 bytes
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
			srv := filesServer(t, fmt.Sprintf(`[{"filename":"a","patch":%q},{"filename":"b","patch":%q}]`, a, b))
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

// Individually small patches that collectively exceed the cap are truncated at
// the file boundary; the diff must not read as complete.
func TestDiffCollectivelyOverCap(t *testing.T) {
	var files []string
	for i := 0; i < 10; i++ {
		files = append(files, fmt.Sprintf(`{"filename":"f%d","patch":"%s"}`, i, strings.Repeat("x", 100)))
	}
	srv := filesServer(t, "["+strings.Join(files, ",")+"]")
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	// 950 bytes fits 9 files (9*100 + 8 separators = 908) but not 10 (1008).
	diff, trunc, err := c.Diff(context.Background(), "o", "r", 1, 950)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !trunc {
		t.Error("trunc = false, want true: the tenth file overflows")
	}
	if len(diff) > 950 {
		t.Errorf("diff len = %d, want <= 950", len(diff))
	}
	if strings.Count(diff, "\n") != 8 {
		t.Errorf("diff has %d separators, want 8 (9 files kept)", strings.Count(diff, "\n"))
	}
}

// A single file larger than the cap cannot be included whole without breaking the
// cap, so the diff is empty and truncated — never a silently complete one.
func TestDiffSingleHugeFile(t *testing.T) {
	srv := filesServer(t, `[{"filename":"big.go","patch":"`+strings.Repeat("z", 2000)+`"}]`)
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	diff, trunc, err := c.Diff(context.Background(), "o", "r", 1, 100)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !trunc {
		t.Error("trunc = false, want true: one file exceeds the cap")
	}
	if diff != "" {
		t.Errorf("diff = %q, want empty: nothing fits under the cap", diff)
	}
}

func TestDiffPaginationTruncatesMidStream(t *testing.T) {
	var srv *httptest.Server
	page2 := `{"filename":"p2","patch":"YYYYYYYYYY"}`
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "page=2") {
			_, _ = fmt.Fprint(w, "["+page2+"]")
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls/1/files?per_page=100&page=2>; rel="next"`, srv.URL))
		_, _ = fmt.Fprint(w, `[{"filename":"p1","patch":"XXXXXXXXXX"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	// page one (10) fits, page two would bring the total to 21, over a 15 cap.
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

func TestDiffFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	if _, _, err := c.Diff(context.Background(), "o", "r", 1, DiffMaxBytes); err == nil {
		t.Fatal("Diff: want error, got nil")
	}
}

func TestDiffRejectsBadArguments(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tt := range []struct {
		number   int
		maxBytes int
	}{
		{0, DiffMaxBytes},
		{-1, DiffMaxBytes},
		{1, 0},
		{1, -5},
	} {
		if _, _, err := c.Diff(context.Background(), "o", "r", tt.number, tt.maxBytes); err == nil {
			t.Errorf("Diff(%d, %d): want error, got nil", tt.number, tt.maxBytes)
		}
	}
}
