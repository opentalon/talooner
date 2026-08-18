package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFileContentReadsTheRefItWasGiven(t *testing.T) {
	var gotPath, gotRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRef = r.URL.Path, r.URL.Query().Get("ref")
		body := base64.StdEncoding.EncodeToString([]byte("rule \"x\" { }\n"))
		// GitHub wraps the payload at 60 columns; the decoder has to cope.
		_, _ = fmt.Fprintf(w, `{"type":"file","size":13,"encoding":"base64","content":"%s\n%s\n"}`,
			body[:4], body[4:])
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	raw, err := c.FileContent(context.Background(), "opentalon", "talooner", ".github/talooner/rules.tln", "master")
	if err != nil {
		t.Fatalf("FileContent: %v", err)
	}
	if string(raw) != "rule \"x\" { }\n" {
		t.Errorf("content = %q", raw)
	}
	if gotPath != "/repos/opentalon/talooner/contents/.github/talooner/rules.tln" {
		t.Errorf("path = %s", gotPath)
	}
	// The ref is the whole fork-safety control: a read that silently defaulted
	// to HEAD would take the ruleset from the branch under review.
	if gotRef != "master" {
		t.Errorf("ref = %q, want master", gotRef)
	}
}

func TestFileContentMissingFileIsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.FileContent(context.Background(), "opentalon", "talooner", "a.tln", "master")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: a repo with no ruleset is an answer, not a failure", err)
	}
}

// A file over the inline limit comes back with no content and encoding "none".
// Reading that as an empty file would be an empty ruleset, which is a valid
// ruleset that approves nothing — a wrong answer rather than a failed run.
func TestFileContentRejectsNonBase64Encoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"type":"file","size":2000000,"encoding":"none","content":""}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.FileContent(context.Background(), "opentalon", "talooner", "big.tln", "master")
	if err == nil {
		t.Fatal("FileContent = nil error, want a failure for an unreadable large file")
	}
	if !strings.Contains(err.Error(), "over the") && !strings.Contains(err.Error(), "encoding") {
		t.Errorf("err = %v, want it to name the size or the encoding", err)
	}
}

func TestFileContentRejectsADirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A directory answers with an array, not an object.
		_, _ = fmt.Fprint(w, `[{"type":"file","name":"rules.tln"}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.FileContent(context.Background(), "opentalon", "talooner", ".github/talooner", "master"); err == nil {
		t.Fatal("FileContent = nil error for a directory")
	}
}

func TestFileContentRefusesTraversalAndEmptyArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: the argument check happens before the call", r.Method, r.URL)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	for _, tt := range []struct{ name, path, ref string }{
		{"traversal", ".github/talooner/../../../etc/passwd", "master"},
		{"dot segment", "./rules.tln", "master"},
		{"empty path", "", "master"},
		{"empty ref", "rules.tln", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.FileContent(context.Background(), "opentalon", "talooner", tt.path, tt.ref); err == nil {
				t.Fatalf("FileContent(%q, %q) = nil error", tt.path, tt.ref)
			}
		})
	}
}

func TestFileContentRejectsUndecodableContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"type":"file","size":4,"encoding":"base64","content":"not base64!!"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.FileContent(context.Background(), "opentalon", "talooner", "a.tln", "master"); err == nil {
		t.Fatal("FileContent = nil error for undecodable content")
	}
}
