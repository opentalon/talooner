package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

func TestGitLabNoteReadsBodyAndAuthor(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = fmt.Fprint(w, `{"id":987,"body":"/talooner plan","author":{"username":"zhisme"}}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	body, author, err := c.Note(context.Background(), "opentalon", "talooner", 7, 987)
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if body != "/talooner plan" {
		t.Errorf("body = %q, want /talooner plan", body)
	}
	if author != "zhisme" {
		t.Errorf("author = %q, want zhisme", author)
	}
	if gotPath != "/projects/opentalon%2Ftalooner/merge_requests/7/notes/987" {
		t.Errorf("path = %s", gotPath)
	}
}

func TestGitLabNoteMissingAuthorIsEmptyString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":987,"body":"a system note","author":null}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, author, err := c.Note(context.Background(), "opentalon", "talooner", 7, 987)
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if author != "" {
		t.Errorf("author = %q, want empty", author)
	}
}

func TestGitLabNoteMissingIsErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"404 Note Not Found"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, _, err := c.Note(context.Background(), "opentalon", "talooner", 7, 987); !errors.Is(err, host.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGitLabNoteRejectsNonPositiveArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: the argument check happens before the call", r.Method, r.URL)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	if _, _, err := c.Note(context.Background(), "opentalon", "talooner", 0, 987); err == nil {
		t.Error("Note with mr iid 0: want error")
	}
	if _, _, err := c.Note(context.Background(), "opentalon", "talooner", 7, 0); err == nil {
		t.Error("Note with note id 0: want error")
	}
}
