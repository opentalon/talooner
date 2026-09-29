package gitlab

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

func TestGitLabFileContentReadsTheRefItWasGiven(t *testing.T) {
	var gotPath, gotRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRef = r.URL.EscapedPath(), r.URL.Query().Get("ref")
		body := base64.StdEncoding.EncodeToString([]byte("rule \"x\" { }\n"))
		_, _ = fmt.Fprintf(w, `{"size":13,"encoding":"base64","content":"%s"}`, body)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	raw, err := c.FileContent(context.Background(), "opentalon", "talooner", ".talooner/rules.tln", "main")
	if err != nil {
		t.Fatalf("FileContent: %v", err)
	}
	if string(raw) != "rule \"x\" { }\n" {
		t.Errorf("content = %q", raw)
	}
	if gotPath != "/projects/opentalon%2Ftalooner/repository/files/.talooner%2Frules.tln" {
		t.Errorf("path = %s", gotPath)
	}
	if gotRef != "main" {
		t.Errorf("ref = %q, want main", gotRef)
	}
}

func TestGitLabFileContentMissingFileIsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"404 File Not Found"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.FileContent(context.Background(), "opentalon", "talooner", "a.tln", "main")
	if !errors.Is(err, host.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound: a repo with no ruleset is an answer, not a failure", err)
	}
}

func TestGitLabFileContentRejectsOversizedFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"size":2000000,"encoding":"base64","content":""}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.FileContent(context.Background(), "opentalon", "talooner", "big.tln", "main"); err == nil {
		t.Fatal("FileContent = nil error, want a failure for a file over the byte cap")
	}
}

func TestGitLabFileContentRejectsNonBase64Encoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"size":2,"encoding":"none","content":"hi"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.FileContent(context.Background(), "opentalon", "talooner", "a.tln", "main"); err == nil {
		t.Fatal("FileContent = nil error, want a failure for a non-base64 encoding")
	}
}

func TestGitLabFileContentRefusesTraversalAndEmptyArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: the argument check happens before the call", r.Method, r.URL)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	for _, tt := range []struct{ name, path, ref string }{
		{"traversal", ".talooner/../../../etc/passwd", "main"},
		{"dot segment", "./rules.tln", "main"},
		{"empty path", "", "main"},
		{"empty ref", "rules.tln", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.FileContent(context.Background(), "opentalon", "talooner", tt.path, tt.ref); err == nil {
				t.Fatalf("FileContent(%q, %q) = nil error", tt.path, tt.ref)
			}
		})
	}
}

func TestGitLabFileContentRejectsUndecodableContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"size":4,"encoding":"base64","content":"not base64!!"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.FileContent(context.Background(), "opentalon", "talooner", "a.tln", "main"); err == nil {
		t.Fatal("FileContent = nil error for undecodable content")
	}
}
