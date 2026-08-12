package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/command"
)

// The client is what B2's gate runs against.
var _ command.PermissionChecker = (*Client)(nil)

func TestPullRequestFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/opentalon/talooner/pulls/42" {
			t.Errorf("path = %s, want /repos/opentalon/talooner/pulls/42", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{
			"number": 42,
			"head": {"sha": "abc123", "ref": "feat/x", "repo": {"full_name": "opentalon/talooner"}},
			"base": {"sha": "def456", "ref": "master", "repo": {"full_name": "opentalon/talooner"}},
			"user": {"login": "evgeny"},
			"title": "Add a thing", "body": "why", "state": "open",
			"draft": true, "additions": 10, "deletions": 3,
			"changed_files": 2, "commits": 4,
			"labels": [{"name": "bug"}, {"name": "v1"}]
		}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	pr, err := c.PullRequest(context.Background(), "opentalon", "talooner", 42)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if pr.HeadSHA != "abc123" || pr.BaseSHA != "def456" {
		t.Errorf("shas = %s/%s, want abc123/def456", pr.HeadSHA, pr.BaseSHA)
	}
	if pr.Author != "evgeny" || !pr.Draft || pr.Commits != 4 || pr.ChangedFiles != 2 {
		t.Errorf("pr = %+v, want author evgeny, draft, 4 commits, 2 changed files", pr)
	}
	if strings.Join(pr.Labels, ",") != "bug,v1" {
		t.Errorf("labels = %v, want [bug v1]", pr.Labels)
	}
	if pr.IsFork {
		t.Error("IsFork = true, want false: head and base are the same repo")
	}
}

func TestPullRequestForkDetection(t *testing.T) {
	for _, tt := range []struct {
		name string
		head string
		want bool
	}{
		{"same repo", `{"sha":"a","repo":{"full_name":"opentalon/talooner"}}`, false},
		{"fork", `{"sha":"a","repo":{"full_name":"someone/talooner"}}`, true},
		{"deleted head repo", `{"sha":"a"}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"number":1,"head":%s,"base":{"sha":"b","repo":{"full_name":"opentalon/talooner"}}}`, tt.head)
			}))
			defer srv.Close()

			c, _ := newTestClient(t, srv)
			pr, err := c.PullRequest(context.Background(), "opentalon", "talooner", 1)
			if err != nil {
				t.Fatalf("PullRequest: %v", err)
			}
			if pr.IsFork != tt.want {
				t.Errorf("IsFork = %v, want %v", pr.IsFork, tt.want)
			}
		})
	}
}

// The reason this call exists at all: an issue_comment payload has no head sha,
// so a PR that comes back without one is a broken answer, not a usable zero.
func TestPullRequestWithoutHeadSHAIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"number":1,"head":{"ref":"feat/x"}}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.PullRequest(context.Background(), "o", "r", 1); err == nil {
		t.Fatal("PullRequest: want error, got nil")
	}
}

func TestPullRequestRejectsBadArguments(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tt := range []struct {
		owner, repo string
		number      int
	}{
		{"", "r", 1},
		{"o", "", 1},
		{"o", "r", 0},
		{"o", "r", -3},
	} {
		if _, err := c.PullRequest(context.Background(), tt.owner, tt.repo, tt.number); err == nil {
			t.Errorf("PullRequest(%q, %q, %d): want error, got nil", tt.owner, tt.repo, tt.number)
		}
	}
}

func TestChangedFilesSinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[{"filename":"go.mod"},{"filename":"internal/github/client.go"},{"filename":""}]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	files, err := c.ChangedFiles(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if want := "go.mod,internal/github/client.go"; strings.Join(files, ",") != want {
		t.Errorf("files = %v, want %s with the empty name dropped", files, want)
	}
}

func TestHasWriteAccessByPermission(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{`{"permission":"admin"}`, true},
		{`{"permission":"write"}`, true},
		{`{"permission":"maintain"}`, true},
		{`{"permission":"WRITE"}`, true},
		{`{"permission":"read"}`, false},
		{`{"permission":"none"}`, false},
		{`{"permission":"read","user":{"role_name":"triage"}}`, false},
		// GitHub reports a maintainer as permission "write" with the finer role
		// in user.role_name; take either.
		{`{"permission":"read","user":{"role_name":"maintain"}}`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if want := "/repos/o/r/collaborators/evgeny/permission"; r.URL.Path != want {
				t.Errorf("path = %s, want %s", r.URL.Path, want)
			}
			_, _ = fmt.Fprint(w, tt.body)
		}))
		c, _ := newTestClient(t, srv)
		got, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
		srv.Close()
		if err != nil {
			t.Fatalf("HasWriteAccess(%s): %v", tt.body, err)
		}
		if got != tt.want {
			t.Errorf("HasWriteAccess(%s) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

// A non-collaborator is a 404 and that is an answer, not a failure.
func TestHasWriteAccessNotACollaborator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	ok, err := c.HasWriteAccess(context.Background(), "o", "r", "drive-by")
	if err != nil {
		t.Fatalf("HasWriteAccess: %v", err)
	}
	if ok {
		t.Error("HasWriteAccess = true, want false")
	}
}

// A broken permission API must fail the run. Reading a 500 as "not authorised"
// would silently drop a maintainer's command.
func TestHasWriteAccessServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(1))
	ok, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
	if err == nil {
		t.Fatal("HasWriteAccess: want error, got nil")
	}
	if ok {
		t.Error("HasWriteAccess = true on a failure, want false")
	}

	// And the gate must report it as a failure rather than as ErrNotAuthorized.
	err = command.Authorize(context.Background(), c, "o", "r", "evgeny")
	if err == nil || errors.Is(err, command.ErrNotAuthorized) {
		t.Errorf("Authorize err = %v, want a plain failure", err)
	}
}

func TestHasWriteAccessEmptyLogin(t *testing.T) {
	c, err := New(testToken)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.HasWriteAccess(context.Background(), "o", "r", ""); err == nil {
		t.Error("HasWriteAccess(\"\"): want error, got nil")
	}
}

// A login is attacker-chosen text on the way into a URL path.
func TestRepoPathEscapesSegments(t *testing.T) {
	got, err := repoPath("o", "r", "collaborators", "../../admin", "permission")
	if err != nil {
		t.Fatalf("repoPath: %v", err)
	}
	if strings.Contains(got, "../") {
		t.Errorf("repoPath = %s, want the traversal escaped", got)
	}
	if _, err := repoPath("o", "r", ""); err == nil {
		t.Error("repoPath with an empty segment: want error, got nil")
	}
}
