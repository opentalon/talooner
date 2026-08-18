package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// checkRunServer is the two endpoints a check run write touches: the list at a
// sha and the create/update. It records every call so a test can assert that a
// second run updated instead of creating a second check run.
type checkRunServer struct {
	mu       sync.Mutex
	posts    []checkRunPayload
	patches  []checkRunPayload
	patchIDs []string
	// existing is the check run already at the sha, if any.
	existing *struct {
		id   int64
		name string
	}
	listStatus  int
	writeStatus int
}

func (s *checkRunServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == http.MethodGet:
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			if s.existing == nil || r.URL.Query().Get("check_name") != s.existing.name {
				_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":%d,"name":%q}]}`,
				s.existing.id, s.existing.name)

		case r.Method == http.MethodPost:
			if s.writeStatus != 0 {
				w.WriteHeader(s.writeStatus)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			s.posts = append(s.posts, decodePayload(t, r))
			_, _ = fmt.Fprint(w, `{"id":991}`)

		case r.Method == http.MethodPatch:
			if s.writeStatus != 0 {
				w.WriteHeader(s.writeStatus)
				_, _ = fmt.Fprint(w, `{"message":"resource not accessible by integration"}`)
				return
			}
			s.patches = append(s.patches, decodePayload(t, r))
			s.patchIDs = append(s.patchIDs, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			_, _ = fmt.Fprintf(w, `{"id":%s}`, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])

		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(testToken, WithBaseURL(srv.URL), WithHTTPClient(srv.Client()), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func decodePayload(t *testing.T, r *http.Request) checkRunPayload {
	t.Helper()
	var p checkRunPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		t.Fatalf("decode %s %s: %v", r.Method, r.URL.Path, err)
	}
	return p
}

func neutralRun() CheckRun {
	return CheckRun{
		Name:       "talooner",
		HeadSHA:    "abc123",
		Conclusion: ConclusionNeutral,
		Title:      "No rules fired",
		Summary:    "No rule matched this pull request.",
	}
}

func TestUpsertCheckRunCreatesWhenThereIsNone(t *testing.T) {
	s := &checkRunServer{}
	id, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", neutralRun())
	if err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if id != 991 {
		t.Errorf("id = %d, want 991", id)
	}
	if len(s.posts) != 1 || len(s.patches) != 0 {
		t.Fatalf("posts = %d, patches = %d, want 1 and 0", len(s.posts), len(s.patches))
	}
	got := s.posts[0]
	if got.Name != "talooner" || got.HeadSHA != "abc123" {
		t.Errorf("identity = %s@%s", got.Name, got.HeadSHA)
	}
	if got.Status != "completed" || got.Conclusion != ConclusionNeutral {
		t.Errorf("status = %q, conclusion = %q", got.Status, got.Conclusion)
	}
	if got.CompletedAt == "" {
		t.Error("a completed check run needs completed_at, or GitHub leaves it in progress forever")
	}
}

// Two runs at the same sha are one check run. A PR with thirty pushes and
// re-runs must not grow thirty talooner checks.
func TestUpsertCheckRunUpdatesInPlace(t *testing.T) {
	s := &checkRunServer{existing: &struct {
		id   int64
		name string
	}{id: 4242, name: "talooner"}}

	cr := neutralRun()
	cr.Conclusion = ConclusionFailure
	cr.Title = "Changes requested"
	id, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", cr)
	if err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if id != 4242 {
		t.Errorf("id = %d, want the existing 4242", id)
	}
	if len(s.posts) != 0 || len(s.patches) != 1 {
		t.Fatalf("posts = %d, patches = %d, want 0 and 1", len(s.posts), len(s.patches))
	}
	if s.patchIDs[0] != "4242" {
		t.Errorf("patched check run %s, want 4242", s.patchIDs[0])
	}
	// head_sha is not sent on an update: it is the identity, not a field.
	if s.patches[0].HeadSHA != "" || s.patches[0].Name != "" {
		t.Errorf("update re-sent the identity: %+v", s.patches[0])
	}
	if s.patches[0].Conclusion != ConclusionFailure {
		t.Errorf("conclusion = %q, want failure", s.patches[0].Conclusion)
	}
}

// A check run of another name at the same sha belongs to somebody else's app.
func TestUpsertCheckRunIgnoresOtherChecks(t *testing.T) {
	s := &checkRunServer{existing: &struct {
		id   int64
		name string
	}{id: 7, name: "build"}}

	if _, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", neutralRun()); err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if len(s.posts) != 1 || len(s.patches) != 0 {
		t.Fatalf("posts = %d, patches = %d, want 1 and 0", len(s.posts), len(s.patches))
	}
}

func TestUpsertCheckRunBatchesAnnotations(t *testing.T) {
	s := &checkRunServer{}
	cr := neutralRun()
	for i := range maxAnnotations + 2 {
		cr.Annotations = append(cr.Annotations, Annotation{
			Path:      ".github/talooner/rules.tln",
			StartLine: i + 1,
			EndLine:   i + 1,
			Level:     LevelFailure,
			Message:   "unexpected token",
		})
	}

	if _, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", cr); err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if len(s.posts) != 1 || len(s.patches) != 1 {
		t.Fatalf("posts = %d, patches = %d, want 1 and 1", len(s.posts), len(s.patches))
	}
	if n := len(s.posts[0].Output.Annotations); n != maxAnnotations {
		t.Errorf("first request carried %d annotations, want the API cap of %d", n, maxAnnotations)
	}
	if n := len(s.patches[0].Output.Annotations); n != 2 {
		t.Errorf("follow-up carried %d annotations, want the remaining 2", n)
	}
	if s.patchIDs[0] != "991" {
		t.Errorf("follow-up patched %s, want the check run just created", s.patchIDs[0])
	}
	first := s.posts[0].Output.Annotations[0]
	if first.AnnotationLevel != LevelFailure || first.StartLine != 1 || first.Path == "" {
		t.Errorf("annotation payload = %+v", first)
	}
}

// A token without checks:write is a run that thinks it wrote a verdict and did
// not. It has to be an error, not a shrug.
func TestUpsertCheckRunSurfacesAWriteFailure(t *testing.T) {
	s := &checkRunServer{writeStatus: http.StatusForbidden}
	_, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", neutralRun())
	if err == nil {
		t.Fatal("a forbidden write must fail the call")
	}
	if !strings.Contains(err.Error(), "talooner") {
		t.Errorf("err = %v, want the check run named", err)
	}
}

func TestUpsertCheckRunSurfacesALookupFailure(t *testing.T) {
	s := &checkRunServer{listStatus: http.StatusInternalServerError}
	if _, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", neutralRun()); err == nil {
		// Falling through to a create would duplicate the check run, which is
		// the one thing this endpoint exists to avoid.
		t.Fatal("a failed lookup must fail the call, not create a second check run")
	}
	if len(s.posts) != 0 {
		t.Errorf("posts = %d, want 0", len(s.posts))
	}
}

func TestUpsertCheckRunRejectsUnwritableRuns(t *testing.T) {
	valid := neutralRun()
	tests := []struct {
		name string
		cr   func(CheckRun) CheckRun
	}{
		{"no name", func(cr CheckRun) CheckRun { cr.Name = ""; return cr }},
		{"no head sha", func(cr CheckRun) CheckRun { cr.HeadSHA = " "; return cr }},
		{"no conclusion", func(cr CheckRun) CheckRun { cr.Conclusion = ""; return cr }},
		{"a conclusion GitHub does not know", func(cr CheckRun) CheckRun { cr.Conclusion = "broken"; return cr }},
		{"no summary", func(cr CheckRun) CheckRun { cr.Summary = ""; return cr }},
		{"an annotation with no path", func(cr CheckRun) CheckRun {
			cr.Annotations = []Annotation{{StartLine: 1, EndLine: 1, Level: LevelFailure, Message: "x"}}
			return cr
		}},
		{"an annotation at line 0", func(cr CheckRun) CheckRun {
			cr.Annotations = []Annotation{{Path: "a.tln", Level: LevelWarning, Message: "x"}}
			return cr
		}},
		{"an annotation ending before it starts", func(cr CheckRun) CheckRun {
			cr.Annotations = []Annotation{{Path: "a.tln", StartLine: 9, EndLine: 2, Level: LevelWarning, Message: "x"}}
			return cr
		}},
		{"an annotation with no message", func(cr CheckRun) CheckRun {
			cr.Annotations = []Annotation{{Path: "a.tln", StartLine: 1, EndLine: 1, Level: LevelWarning}}
			return cr
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &checkRunServer{}
			if _, err := s.client(t).UpsertCheckRun(context.Background(), "opentalon", "talooner", tt.cr(valid)); err == nil {
				t.Fatal("want an error before any request is made")
			}
			if len(s.posts)+len(s.patches) != 0 {
				t.Errorf("an invalid check run reached GitHub: %+v %+v", s.posts, s.patches)
			}
		})
	}
}
