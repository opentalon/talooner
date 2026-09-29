package gitlab

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

type onboardServer struct {
	mu sync.Mutex

	existing map[string]bool

	puts  []ciVariable
	posts []ciVariable
	mrs   []mergeRequest

	putStatus  int
	postStatus int
	mrStatus   int
}

func (s *onboardServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/variables/") && r.Method == http.MethodPut:
			key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if s.putStatus != 0 {
				w.WriteHeader(s.putStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			if !s.existing[key] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"message":"404 Variable Not Found"}`)
				return
			}
			var v ciVariable
			_ = json.NewDecoder(r.Body).Decode(&v)
			s.puts = append(s.puts, v)
			_, _ = fmt.Fprint(w, `{}`)

		case strings.HasSuffix(r.URL.Path, "/variables") && r.Method == http.MethodPost:
			if s.postStatus != 0 {
				w.WriteHeader(s.postStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var v ciVariable
			_ = json.NewDecoder(r.Body).Decode(&v)
			s.posts = append(s.posts, v)
			if s.existing == nil {
				s.existing = map[string]bool{}
			}
			s.existing[v.Key] = true
			_, _ = fmt.Fprint(w, `{}`)

		case strings.HasSuffix(r.URL.Path, "/merge_requests") && r.Method == http.MethodPost:
			if s.mrStatus != 0 {
				w.WriteHeader(s.mrStatus)
				_, _ = fmt.Fprint(w, `{"message":"boom"}`)
				return
			}
			var mr mergeRequest
			_ = json.NewDecoder(r.Body).Decode(&mr)
			s.mrs = append(s.mrs, mr)
			mr.WebURL = "https://gitlab.example.com/acme/api/-/merge_requests/1"
			raw, err := json.Marshal(mr)
			if err != nil {
				t.Errorf("encode mr: %v", err)
			}
			_, _ = w.Write(raw)

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"unexpected path"}`)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New("token", WithBaseURL(srv.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestUpsertProjectVariableCreatesWhenMissing(t *testing.T) {
	s := &onboardServer{}
	c := s.client(t)
	if err := c.UpsertProjectVariable(context.Background(), "acme", "api", "OPENTALON_HOST", "grpc://x:9090", false); err != nil {
		t.Fatalf("UpsertProjectVariable: %v", err)
	}
	if len(s.puts) != 0 {
		t.Errorf("puts = %v, want none (variable didn't exist yet)", s.puts)
	}
	if len(s.posts) != 1 || s.posts[0].Key != "OPENTALON_HOST" || s.posts[0].Value != "grpc://x:9090" || s.posts[0].Masked {
		t.Errorf("posts = %+v, want one unmasked OPENTALON_HOST post", s.posts)
	}
}

func TestUpsertProjectVariableUpdatesWhenPresent(t *testing.T) {
	s := &onboardServer{existing: map[string]bool{"OPENTALON_API_KEY": true}}
	c := s.client(t)
	if err := c.UpsertProjectVariable(context.Background(), "acme", "api", "OPENTALON_API_KEY", "otk_secret", true); err != nil {
		t.Fatalf("UpsertProjectVariable: %v", err)
	}
	if len(s.posts) != 0 {
		t.Errorf("posts = %v, want none (variable already existed)", s.posts)
	}
	if len(s.puts) != 1 || s.puts[0].Key != "OPENTALON_API_KEY" || s.puts[0].Value != "otk_secret" || !s.puts[0].Masked {
		t.Errorf("puts = %+v, want one masked OPENTALON_API_KEY put", s.puts)
	}
}

func TestUpsertGroupVariableUsesGroupPath(t *testing.T) {
	s := &onboardServer{}
	c := s.client(t)
	if err := c.UpsertGroupVariable(context.Background(), "acme", "OPENTALON_HOST", "grpc://x:9090", false); err != nil {
		t.Fatalf("UpsertGroupVariable: %v", err)
	}
	if len(s.posts) != 1 {
		t.Fatalf("posts = %v, want one", s.posts)
	}
}

func TestUpsertVariableRejectsEmptyKey(t *testing.T) {
	s := &onboardServer{}
	c := s.client(t)
	if err := c.UpsertProjectVariable(context.Background(), "acme", "api", "", "v", false); err == nil {
		t.Error("UpsertProjectVariable with empty key: want error, got nil")
	}
	if err := c.UpsertGroupVariable(context.Background(), "acme", "", "v", false); err == nil {
		t.Error("UpsertGroupVariable with empty key: want error, got nil")
	}
}

func TestUpsertVariablePropagatesServerError(t *testing.T) {
	s := &onboardServer{postStatus: http.StatusInternalServerError}
	c := s.client(t)
	if err := c.UpsertProjectVariable(context.Background(), "acme", "api", "OPENTALON_HOST", "v", false); err == nil {
		t.Error("UpsertProjectVariable with 500 on create: want error, got nil")
	}
}

func TestUpsertVariablePropagatesUpdateError(t *testing.T) {
	s := &onboardServer{existing: map[string]bool{"OPENTALON_HOST": true}, putStatus: http.StatusForbidden}
	c := s.client(t)
	if err := c.UpsertProjectVariable(context.Background(), "acme", "api", "OPENTALON_HOST", "v", false); err == nil {
		t.Error("UpsertProjectVariable with 403 on update: want error, got nil")
	}
}

func TestCreateMergeRequestHappyPath(t *testing.T) {
	s := &onboardServer{}
	c := s.client(t)
	url, err := c.CreateMergeRequest(context.Background(), "acme", "api", "talooner-onboarding", "master", "talooner onboarding", "body text")
	if err != nil {
		t.Fatalf("CreateMergeRequest: %v", err)
	}
	if url != "https://gitlab.example.com/acme/api/-/merge_requests/1" {
		t.Errorf("url = %q, want the created MR's web_url", url)
	}
	if len(s.mrs) != 1 || s.mrs[0].SourceBranch != "talooner-onboarding" || s.mrs[0].TargetBranch != "master" || s.mrs[0].Title != "talooner onboarding" {
		t.Errorf("mrs = %+v, want one matching the request", s.mrs)
	}
}

func TestCreateMergeRequestRejectsEmptyBranchesOrTitle(t *testing.T) {
	s := &onboardServer{}
	c := s.client(t)
	cases := []struct{ source, target, title string }{
		{"", "master", "t"},
		{"branch", "", "t"},
		{"branch", "master", ""},
	}
	for _, tc := range cases {
		if _, err := c.CreateMergeRequest(context.Background(), "acme", "api", tc.source, tc.target, tc.title, ""); err == nil {
			t.Errorf("CreateMergeRequest(%+v): want error, got nil", tc)
		}
	}
	if len(s.mrs) != 0 {
		t.Errorf("mrs = %v, want none created for rejected input", s.mrs)
	}
}

func TestCreateMergeRequestPropagatesServerError(t *testing.T) {
	s := &onboardServer{mrStatus: http.StatusUnprocessableEntity}
	c := s.client(t)
	if _, err := c.CreateMergeRequest(context.Background(), "acme", "api", "branch", "master", "title", ""); err == nil {
		t.Error("CreateMergeRequest with 422: want error, got nil")
	}
}
