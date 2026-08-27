package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const marker = "<!-- talooner:v1:review -->"

// commentServer is the three endpoints a sticky comment write touches: the
// listing, the create and the edit. It records every write so a test can assert
// that a second run edited instead of posting a second comment.
type commentServer struct {
	mu       sync.Mutex
	posts    []string
	patches  []string
	patchIDs []int64
	// existing is what the listing returns, i.e. the comments already on the PR.
	existing []issueComment
	// gone is the id whose edit answers 404, i.e. a comment deleted between the
	// listing and the write.
	gone        int64
	listStatus  int
	writeStatus int
}

func (s *commentServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			raw, err := json.Marshal(s.existing)
			if err != nil {
				t.Errorf("encode listing: %v", err)
			}
			_, _ = w.Write(raw)

		case http.MethodPost:
			if s.writeStatus != 0 {
				w.WriteHeader(s.writeStatus)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			s.posts = append(s.posts, body(t, r))
			_, _ = fmt.Fprint(w, `{"id":555}`)

		case http.MethodPatch:
			id, err := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
			if err != nil {
				t.Errorf("patch to %s carries no comment id", r.URL.Path)
			}
			if id == s.gone {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			if s.writeStatus != 0 {
				w.WriteHeader(s.writeStatus)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			s.patchIDs = append(s.patchIDs, id)
			s.patches = append(s.patches, body(t, r))
			_, _ = fmt.Fprintf(w, `{"id":%d}`, id)

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New("ghs_test", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func body(t *testing.T, r *http.Request) string {
	t.Helper()
	var payload issueComment
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decode comment body: %v", err)
	}
	return payload.Body
}

func sticky(b string) StickyComment { return StickyComment{Marker: marker, Body: b} }

func TestFirstRunPostsTheComment(t *testing.T) {
	s := &commentServer{}
	id, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky("findings"))
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 555 {
		t.Errorf("id = %d, want 555", id)
	}
	if len(s.posts) != 1 || len(s.patches) != 0 {
		t.Fatalf("posts = %d, patches = %d, want 1 and 0", len(s.posts), len(s.patches))
	}
	if !strings.HasPrefix(s.posts[0], marker+"\n") {
		t.Errorf("posted body does not start with the marker: %q", s.posts[0])
	}
	if !strings.Contains(s.posts[0], "findings") {
		t.Errorf("posted body lost the text: %q", s.posts[0])
	}
}

func TestSecondRunEditsInsteadOfPosting(t *testing.T) {
	s := &commentServer{existing: []issueComment{
		{ID: 1, Body: "a human said something"},
		{ID: 2, Body: marker + "\nolder findings"},
	}}
	id, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky("new findings"))
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 2 {
		t.Errorf("id = %d, want 2", id)
	}
	if len(s.posts) != 0 || len(s.patches) != 1 {
		t.Fatalf("posts = %d, patches = %d, want 0 and 1", len(s.posts), len(s.patches))
	}
	if !strings.Contains(s.patches[0], "new findings") {
		t.Errorf("edited body = %q, want the new findings", s.patches[0])
	}
}

// A maintainer deleting the comment must not break the next run, and must not
// make it 404 on an id it remembered.
func TestADeletedCommentIsPostedAgain(t *testing.T) {
	s := &commentServer{existing: []issueComment{{ID: 1, Body: "unrelated"}}}
	if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky("findings")); err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if len(s.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(s.posts))
	}
}

// Deleted between the listing and the edit: the edit 404s and the write falls
// back to a create, which is the same outcome as having seen it deleted.
func TestACommentDeletedMidWriteIsPostedAgain(t *testing.T) {
	s := &commentServer{
		existing: []issueComment{{ID: 9, Body: marker + "\nold"}},
		gone:     9,
	}
	id, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky("findings"))
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 555 {
		t.Errorf("id = %d, want the newly posted 555", id)
	}
	if len(s.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(s.posts))
	}
}

// Two markers, from a botched earlier run: edit one, deterministically, and do
// not add a third.
func TestTwoMarkersEditTheOldestAndDoNotFanOut(t *testing.T) {
	s := &commentServer{existing: []issueComment{
		{ID: 30, Body: marker + "\nsecond"},
		{ID: 10, Body: marker + "\nfirst"},
		{ID: 20, Body: "a human"},
	}}
	for range 2 {
		if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky("findings")); err != nil {
			t.Fatalf("UpsertComment: %v", err)
		}
	}
	if len(s.posts) != 0 {
		t.Fatalf("posts = %d, want 0: a duplicate must never be added to", len(s.posts))
	}
	for _, id := range s.patchIDs {
		if id != 10 {
			t.Errorf("edited comment %d, want the oldest, 10", id)
		}
	}
}

// EditOnly is how a topic is retired: it must not introduce a comment on a PR
// that never had one.
func TestEditOnlyPostsNothingWhenThereIsNoComment(t *testing.T) {
	s := &commentServer{existing: []issueComment{{ID: 1, Body: "unrelated"}}}
	id, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42,
		StickyComment{Marker: marker, Body: "resolved", EditOnly: true})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0", id)
	}
	if len(s.posts) != 0 || len(s.patches) != 0 {
		t.Fatalf("posts = %d, patches = %d, want nothing written", len(s.posts), len(s.patches))
	}
}

func TestEditOnlyEditsWhenTheCommentIsThere(t *testing.T) {
	s := &commentServer{existing: []issueComment{{ID: 7, Body: marker + "\nfindings"}}}
	id, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42,
		StickyComment{Marker: marker, Body: "resolved", EditOnly: true})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 7 || len(s.patches) != 1 {
		t.Fatalf("id = %d, patches = %d, want 7 and 1", id, len(s.patches))
	}
}

// A comment retired at the same moment somebody deleted it stays deleted. The
// fallback create only exists for the topics that have something to say.
func TestEditOnlyDoesNotResurrectADeletedComment(t *testing.T) {
	s := &commentServer{
		existing: []issueComment{{ID: 9, Body: marker + "\nold"}},
		gone:     9,
	}
	if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42,
		StickyComment{Marker: marker, Body: "resolved", EditOnly: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(s.posts) != 0 {
		t.Fatalf("posts = %d, want 0", len(s.posts))
	}
}

// A failed listing must not fall through to a create: that is the duplicate the
// marker exists to prevent.
func TestAFailedListingDoesNotPost(t *testing.T) {
	s := &commentServer{listStatus: http.StatusInternalServerError}
	if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky("findings")); err == nil {
		t.Fatal("UpsertComment succeeded, want the listing failure")
	}
	if len(s.posts) != 0 {
		t.Fatalf("posts = %d, want 0", len(s.posts))
	}
}

func TestAnOversizedBodyIsTruncatedRatherThanRejected(t *testing.T) {
	s := &commentServer{}
	long := strings.Repeat("é", maxCommentBytes) // two bytes each: over the cap either way
	if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, sticky(long)); err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	got := s.posts[0]
	if len(got) > maxCommentBytes {
		t.Errorf("posted %d bytes, over the %d cap", len(got), maxCommentBytes)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), strings.TrimRight(truncationNotice, "\n")) {
		t.Errorf("truncated body does not say so: %q", got[len(got)-120:])
	}
	if !strings.HasPrefix(got, marker) {
		t.Error("truncation lost the marker, so the next run would post a second comment")
	}
	if !utf8.ValidString(got) {
		t.Error("truncation cut a rune in half")
	}
}

func TestCommentValidation(t *testing.T) {
	tests := []struct {
		name string
		s    StickyComment
	}{
		{"no marker", StickyComment{Body: "x"}},
		{"marker spanning lines", StickyComment{Marker: "<!-- a\nb -->", Body: "x"}},
		{"no body", StickyComment{Marker: marker, Body: "  \n"}},
		// A body carrying the marker would make the next run's listing match on
		// text the renderer put there, and the truncation cap moves.
		{"marker inside the body", StickyComment{Marker: marker, Body: "look: " + marker}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &commentServer{}
			if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 42, tt.s); err == nil {
				t.Fatal("UpsertComment succeeded, want a validation failure")
			}
		})
	}
}

func TestCommentNeedsAPullRequestNumber(t *testing.T) {
	s := &commentServer{}
	if _, err := s.client(t).UpsertComment(t.Context(), "opentalon", "talooner", 0, sticky("x")); err == nil {
		t.Fatal("UpsertComment succeeded on PR 0")
	}
}

// CreateComment never lists first — every call posts, unlike UpsertComment's
// listen-then-post-or-edit — because it never looks for a comment to edit.
func TestCreateCommentAlwaysPosts(t *testing.T) {
	s := &commentServer{existing: []issueComment{{ID: 1, Body: marker + "\nold"}}}
	id, err := s.client(t).CreateComment(t.Context(), "opentalon", "talooner", 42, "the answer")
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if id != 555 {
		t.Errorf("id = %d, want 555", id)
	}
	if len(s.posts) != 1 || s.posts[0] != "the answer" {
		t.Errorf("posts = %v, want exactly one carrying the body verbatim", s.posts)
	}
	if len(s.patches) != 0 {
		t.Error("CreateComment edited an existing comment; it must never look one up")
	}
}

// Two calls are two comments — the whole point of CreateComment over the
// sticky writer, for a reply that answers one specific ask.
func TestCreateCommentNeverDeduplicates(t *testing.T) {
	s := &commentServer{}
	c := s.client(t)
	if _, err := c.CreateComment(t.Context(), "opentalon", "talooner", 42, "first answer"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if _, err := c.CreateComment(t.Context(), "opentalon", "talooner", 42, "second answer"); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if len(s.posts) != 2 {
		t.Fatalf("posts = %d, want 2: a later ask must not overwrite an earlier answer", len(s.posts))
	}
}

func TestCreateCommentTruncatesAnOversizedBody(t *testing.T) {
	s := &commentServer{}
	long := strings.Repeat("é", maxCommentBytes)
	if _, err := s.client(t).CreateComment(t.Context(), "opentalon", "talooner", 42, long); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	got := s.posts[0]
	if len(got) > maxCommentBytes {
		t.Errorf("posted %d bytes, over the %d cap", len(got), maxCommentBytes)
	}
	if !utf8.ValidString(got) {
		t.Error("truncation cut a rune in half")
	}
}

func TestCreateCommentRejectsAnEmptyBody(t *testing.T) {
	s := &commentServer{}
	if _, err := s.client(t).CreateComment(t.Context(), "opentalon", "talooner", 42, "  \n"); err == nil {
		t.Fatal("CreateComment succeeded with a blank body")
	}
	if len(s.posts) != 0 {
		t.Error("a blank body reached the API")
	}
}

func TestCreateCommentNeedsAPullRequestNumber(t *testing.T) {
	s := &commentServer{}
	if _, err := s.client(t).CreateComment(t.Context(), "opentalon", "talooner", 0, "x"); err == nil {
		t.Fatal("CreateComment succeeded on PR 0")
	}
}

func TestCreateCommentFailsOnAWriteError(t *testing.T) {
	s := &commentServer{writeStatus: http.StatusForbidden}
	if _, err := s.client(t).CreateComment(t.Context(), "opentalon", "talooner", 42, "x"); err == nil {
		t.Fatal("CreateComment succeeded despite the write failing")
	}
}
