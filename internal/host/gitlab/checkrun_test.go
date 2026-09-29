package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/opentalon/talooner/internal/host"
)

var _ host.Host = (*Client)(nil)

func TestGitLabCommitStatusOfMapsConclusions(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{host.ConclusionSuccess, "success", false},
		{host.ConclusionNeutral, "success", false},
		{host.ConclusionFailure, "failed", false},
		{"bogus", "", true},
	} {
		got, err := commitStatusOf(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("commitStatusOf(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("commitStatusOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func neutralCheckRun() host.CheckRun {
	return host.CheckRun{
		Name:       "talooner",
		HeadSHA:    "abc123",
		Conclusion: host.ConclusionNeutral,
		Title:      "No issues found",
		Summary:    "Nothing matched.",
	}
}

func TestGitLabUpsertCheckRunPosts(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody commitStatusWrite
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":991}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	id, err := c.UpsertCheckRun(context.Background(), "o", "r", neutralCheckRun())
	if err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if id != 991 {
		t.Errorf("id = %d, want 991", id)
	}
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/statuses/abc123") {
		t.Errorf("%s %s, want POST .../statuses/abc123", gotMethod, gotPath)
	}
	if gotBody.State != "success" || gotBody.Name != "talooner" {
		t.Errorf("body = %+v, want state success, name talooner", gotBody)
	}
	if !strings.Contains(gotBody.Description, "No issues found") {
		t.Errorf("description = %q, want it to carry the title", gotBody.Description)
	}
}

func TestGitLabUpsertCheckRunFoldsAnnotationCount(t *testing.T) {
	cr := neutralCheckRun()
	cr.Annotations = []host.Annotation{
		{Path: "a.tln", StartLine: 1, EndLine: 1, Level: host.LevelFailure, Message: "boom"},
		{Path: "b.tln", StartLine: 2, EndLine: 2, Level: host.LevelFailure, Message: "bang"},
	}

	var gotBody commitStatusWrite
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.UpsertCheckRun(context.Background(), "o", "r", cr); err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if !strings.Contains(gotBody.Description, "2 annotation(s)") {
		t.Errorf("description = %q, want it to name the annotation count GitLab has no per-line surface for",
			gotBody.Description)
	}
}

func TestGitLabUpsertCheckRunTruncatesOversizedFields(t *testing.T) {
	cr := neutralCheckRun()
	cr.Title = strings.Repeat("t", 300)
	cr.DetailsURL = "https://example.com/" + strings.Repeat("x", 300)

	var gotBody commitStatusWrite
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.UpsertCheckRun(context.Background(), "o", "r", cr); err != nil {
		t.Fatalf("UpsertCheckRun: %v", err)
	}
	if n := utf8.RuneCountInString(gotBody.Description); n > maxStatusFieldRunes {
		t.Errorf("description = %d runes, over GitLab's %d cap", n, maxStatusFieldRunes)
	}
	if n := utf8.RuneCountInString(gotBody.TargetURL); n > maxStatusFieldRunes {
		t.Errorf("target_url = %d runes, over GitLab's %d cap", n, maxStatusFieldRunes)
	}
}

func TestGitLabUpsertCheckRunRejectsMissingNameOrSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: the argument check happens before the call", r.Method, r.URL)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	for _, tt := range []struct {
		name string
		cr   host.CheckRun
	}{
		{"no name", host.CheckRun{HeadSHA: "abc123", Conclusion: host.ConclusionSuccess}},
		{"no sha", host.CheckRun{Name: "talooner", Conclusion: host.ConclusionSuccess}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.UpsertCheckRun(context.Background(), "o", "r", tt.cr); err == nil {
				t.Fatal("UpsertCheckRun succeeded despite missing identity")
			}
		})
	}
}

func TestGitLabUpsertCheckRunRejectsUnknownConclusion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	cr := neutralCheckRun()
	cr.Conclusion = "bogus"
	if _, err := c.UpsertCheckRun(context.Background(), "o", "r", cr); err == nil {
		t.Fatal("UpsertCheckRun succeeded with an unmapped conclusion")
	}
}

func TestGitLabUpsertCheckRunFailsOnAWriteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"nope"}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.UpsertCheckRun(context.Background(), "o", "r", neutralCheckRun()); err == nil {
		t.Fatal("UpsertCheckRun succeeded despite the write failing")
	}
}
