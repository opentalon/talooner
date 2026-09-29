package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

const testCommentMarker = "<!-- talooner:v1:ledger -->"

type commentServer struct {
	mu sync.Mutex

	notes []note

	posted []string
	edited map[int64]string

	listStatus int
	postStatus int
	editStatus int
}

func (s *commentServer) client(t *testing.T) *Client {
	t.Helper()
	if s.edited == nil {
		s.edited = map[int64]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/notes") && r.Method == http.MethodGet:
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			raw, err := json.Marshal(s.notes)
			if err != nil {
				t.Errorf("encode notes: %v", err)
			}
			_, _ = w.Write(raw)

		case strings.HasSuffix(r.URL.Path, "/notes") && r.Method == http.MethodPost:
			if s.postStatus != 0 {
				w.WriteHeader(s.postStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode note: %v", err)
			}
			s.posted = append(s.posted, body.Body)
			id := int64(len(s.notes) + len(s.posted) + 1000)
			s.notes = append(s.notes, note{ID: id, Body: body.Body})
			_, _ = fmt.Fprintf(w, `{"id":%d,"body":%s}`, id, jsonString(body.Body))

		case r.Method == http.MethodPut:
			if s.editStatus != 0 {
				w.WriteHeader(s.editStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			parts := strings.Split(r.URL.Path, "/")
			var id int64
			_, _ = fmt.Sscanf(parts[len(parts)-1], "%d", &id)
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode edit: %v", err)
			}
			s.edited[id] = body.Body
			_, _ = fmt.Fprintf(w, `{"id":%d,"body":%s}`, id, jsonString(body.Body))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	c, _ := newTestClient(t, srv)
	t.Cleanup(srv.Close)
	return c
}

func TestUpsertCommentPostsWhenNoneExists(t *testing.T) {
	s := &commentServer{}
	c := s.client(t)

	id, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker: testCommentMarker,
		Body:   "hello",
	})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id == 0 {
		t.Fatal("UpsertComment returned id 0")
	}
	if len(s.posted) != 1 || !strings.Contains(s.posted[0], "hello") {
		t.Errorf("posted = %v, want one note containing hello", s.posted)
	}
}

func TestUpsertCommentEditsExisting(t *testing.T) {
	s := &commentServer{notes: []note{{ID: 42, Body: testCommentMarker + "\nold"}}}
	c := s.client(t)

	id, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker: testCommentMarker,
		Body:   "new",
	})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42 (edited, not reposted)", id)
	}
	if got := s.edited[42]; !strings.Contains(got, "new") {
		t.Errorf("edited[42] = %q, want it to contain new", got)
	}
	if len(s.posted) != 0 {
		t.Errorf("posted = %v, want no new note when editing", s.posted)
	}
}

func TestUpsertCommentEditOnlySkipsPostWhenMissing(t *testing.T) {
	s := &commentServer{}
	c := s.client(t)

	id, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker:   testCommentMarker,
		Body:     "hello",
		EditOnly: true,
	})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0 (edit-only, nothing to edit)", id)
	}
	if len(s.posted) != 0 {
		t.Errorf("posted = %v, want no note posted", s.posted)
	}
}

func TestUpsertCommentPostsNewWhenExistingDisappearsMidEdit(t *testing.T) {
	s := &commentServer{notes: []note{{ID: 42, Body: testCommentMarker + "\nold"}}, editStatus: http.StatusNotFound}
	c := s.client(t)

	id, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker: testCommentMarker,
		Body:   "new",
	})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id == 0 || id == 42 {
		t.Errorf("id = %d, want a freshly posted id", id)
	}
	if len(s.posted) != 1 {
		t.Errorf("posted = %v, want a fallback post", s.posted)
	}
}

func TestUpsertCommentRejectsMarkerlessComment(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{Body: "hi"}); err == nil {
		t.Error("UpsertComment with no marker: want error, got nil")
	}
}

func TestUpsertCommentRejectsBodyContainingMarker(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker: testCommentMarker,
		Body:   "see " + testCommentMarker,
	}); err == nil {
		t.Error("UpsertComment with marker in body: want error, got nil")
	}
}

func TestUpsertCommentRejectsNonPositiveNumber(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.UpsertComment(context.Background(), "o", "r", 0, host.StickyComment{
		Marker: testCommentMarker, Body: "hi",
	}); err == nil {
		t.Error("UpsertComment with number 0: want error, got nil")
	}
}

func TestUpsertCommentKeepsOldestOnDuplicateMarkers(t *testing.T) {
	s := &commentServer{notes: []note{
		{ID: 99, Body: testCommentMarker + "\nfirst"},
		{ID: 42, Body: testCommentMarker + "\nsecond"},
	}}
	c := s.client(t)

	id, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker: testCommentMarker,
		Body:   "third",
	})
	if err != nil {
		t.Fatalf("UpsertComment: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42 (the lower/oldest id)", id)
	}
}

func TestCommentBodyReturnsMatchingNote(t *testing.T) {
	s := &commentServer{notes: []note{{ID: 42, Body: testCommentMarker + "\npayload"}}}
	c := s.client(t)

	body, err := c.CommentBody(context.Background(), "o", "r", 5, testCommentMarker)
	if err != nil {
		t.Fatalf("CommentBody: %v", err)
	}
	if !strings.Contains(body, "payload") {
		t.Errorf("body = %q, want it to contain payload", body)
	}
}

func TestCommentBodyEmptyWhenNoMatch(t *testing.T) {
	s := &commentServer{}
	c := s.client(t)

	body, err := c.CommentBody(context.Background(), "o", "r", 5, testCommentMarker)
	if err != nil {
		t.Fatalf("CommentBody: %v", err)
	}
	if body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestCommentBodyRejectsEmptyMarker(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.CommentBody(context.Background(), "o", "r", 5, ""); err == nil {
		t.Error("CommentBody with empty marker: want error, got nil")
	}
}

func TestCommentBodySkipsSystemNotes(t *testing.T) {
	s := &commentServer{notes: []note{{ID: 42, Body: testCommentMarker + "\nsystem note", System: true}}}
	c := s.client(t)

	body, err := c.CommentBody(context.Background(), "o", "r", 5, testCommentMarker)
	if err != nil {
		t.Fatalf("CommentBody: %v", err)
	}
	if body != "" {
		t.Errorf("body = %q, want empty (system notes must not match)", body)
	}
}

func TestUpsertCommentPropagatesListFailure(t *testing.T) {
	s := &commentServer{listStatus: http.StatusInternalServerError}
	c := s.client(t)

	if _, err := c.UpsertComment(context.Background(), "o", "r", 5, host.StickyComment{
		Marker: testCommentMarker, Body: "hi",
	}); !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
}

func TestTruncateLongCommentKeepsValidUTF8AndNotice(t *testing.T) {
	s := host.StickyComment{Marker: testCommentMarker, Body: strings.Repeat("a", maxCommentBytes)}
	text := stickyCommentText(s)
	if len(text) > maxCommentBytes {
		t.Errorf("truncated length = %d, want <= %d", len(text), maxCommentBytes)
	}
	if !strings.Contains(text, "truncated") {
		t.Error("truncated text missing truncation notice")
	}
}
