package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/talooner/internal/host"
)

func TestProjectPathEscapesNamespace(t *testing.T) {
	path, err := projectPath("my-org", "my repo", "merge_requests", "7")
	if err != nil {
		t.Fatalf("projectPath: %v", err)
	}
	if want := "/projects/my-org%2Fmy%20repo/merge_requests/7"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
}

func TestProjectPathRejectsEmptyOwnerOrRepo(t *testing.T) {
	if _, err := projectPath("", "r"); err == nil {
		t.Error("projectPath with empty owner: want error, got nil")
	}
	if _, err := projectPath("o", ""); err == nil {
		t.Error("projectPath with empty repo: want error, got nil")
	}
}

func TestPullRequestMapsCoreFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{
			"iid": 42,
			"source_project_id": 2,
			"target_project_id": 3,
			"title": "Add feature",
			"description": "does the thing",
			"state": "opened",
			"source_branch": "feat",
			"target_branch": "main",
			"draft": true,
			"merge_status": "can_be_merged",
			"sha": "headsha123",
			"diff_refs": {"base_sha": "basesha456", "head_sha": "headsha123"},
			"author": {"username": "evgeny"},
			"labels": ["bug"],
			"assignees": [{"username": "alice"}],
			"reviewers": [{"username": "bob"}]
		}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	pr, err := c.PullRequest(context.Background(), "o", "r", 42)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.HeadSHA != "headsha123" || pr.BaseSHA != "basesha456" {
		t.Errorf("HeadSHA/BaseSHA = %s/%s, want headsha123/basesha456", pr.HeadSHA, pr.BaseSHA)
	}
	if pr.HeadRef != "feat" || pr.BaseRef != "main" {
		t.Errorf("HeadRef/BaseRef = %s/%s, want feat/main", pr.HeadRef, pr.BaseRef)
	}
	if pr.Author != "evgeny" {
		t.Errorf("Author = %s, want evgeny", pr.Author)
	}
	if pr.State != "open" {
		t.Errorf("State = %s, want open", pr.State)
	}
	if !pr.Draft {
		t.Error("Draft = false, want true")
	}
	if pr.Merged {
		t.Error("Merged = true, want false")
	}
	if pr.Mergeable == nil || !*pr.Mergeable {
		t.Errorf("Mergeable = %v, want true", pr.Mergeable)
	}
	if !pr.IsFork {
		t.Error("IsFork = false, want true: source_project_id != target_project_id")
	}
	if len(pr.Assignees) != 1 || pr.Assignees[0] != "alice" {
		t.Errorf("Assignees = %v, want [alice]", pr.Assignees)
	}
	if len(pr.Requested.Users) != 1 || pr.Requested.Users[0] != "bob" {
		t.Errorf("Requested.Users = %v, want [bob]", pr.Requested.Users)
	}
}

func TestPullRequestRejectsMissingHeadSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"iid": 1, "state": "opened"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	if _, err := c.PullRequest(context.Background(), "o", "r", 1); err == nil {
		t.Error("PullRequest with no head sha: want error, got nil")
	}
}

func TestPullRequestRejectsNonPositiveNumber(t *testing.T) {
	c, _ := New(testToken)
	for _, n := range []int{0, -1} {
		if _, err := c.PullRequest(context.Background(), "o", "r", n); err == nil {
			t.Errorf("PullRequest(%d): want error, got nil", n)
		}
	}
}

func TestMRStateMapsMergedAndClosedToClosed(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"opened", "open"},
		{"closed", "closed"},
		{"merged", "closed"},
		{"locked", "closed"},
	} {
		if got := mrState(tt.in); got != tt.want {
			t.Errorf("mrState(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMRMergeablePrefersDetailedStatus(t *testing.T) {
	for _, tt := range []struct {
		detailed, legacy string
		want             *bool
	}{
		{"mergeable", "cannot_be_merged", boolPtr(true)},
		{"conflict", "can_be_merged", boolPtr(false)},
		{"checking", "can_be_merged", nil},
		{"", "can_be_merged", boolPtr(true)},
		{"", "cannot_be_merged", boolPtr(false)},
		{"", "unchecked", nil},
		{"", "", nil},
	} {
		got := mrMergeable(tt.detailed, tt.legacy)
		if !boolPtrEqual(got, tt.want) {
			t.Errorf("mrMergeable(%q, %q) = %v, want %v", tt.detailed, tt.legacy, got, tt.want)
		}
	}
}

func TestResolveMergeablePollsUntilDecided(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		status := "checking"
		if calls >= 3 {
			status = "mergeable"
		}
		_, _ = fmt.Fprintf(w, `{"iid":1,"sha":"s","state":"opened","detailed_merge_status":%q}`, status)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv)
	pr, err := c.ResolveMergeable(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("ResolveMergeable: %v", err)
	}
	if pr.Mergeable == nil || !*pr.Mergeable {
		t.Errorf("Mergeable = %v, want true", pr.Mergeable)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if len(*waits) != 2 {
		t.Errorf("waits = %v, want 2 polling waits", *waits)
	}
}

func TestDiffStatCountsAcrossHeaderStyles(t *testing.T) {
	for _, tt := range []struct {
		name     string
		diff     string
		add, del int
	}{
		{
			name: "no file header",
			diff: "@@ -1,2 +1,3 @@\n context\n-old line\n+new line\n+another new line\n",
			add:  2, del: 1,
		},
		{
			name: "with file header",
			diff: "--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-old\n+new\n",
			add:  1, del: 1,
		},
		{
			name: "no changes, rename only",
			diff: "",
			add:  0, del: 0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			add, del := diffStat(tt.diff)
			if add != tt.add || del != tt.del {
				t.Errorf("diffStat = %d/%d, want %d/%d", add, del, tt.add, tt.del)
			}
		})
	}
}

func TestChangedFileStatsUsesNewPathAndFallsBackToOldPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[
			{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-x\n+y\n"},
			{"old_path":"b.go","new_path":"","deleted_file":true,"diff":"@@ -1 +0,0 @@\n-gone\n"}
		]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	stats, err := c.ChangedFileStats(context.Background(), "o", "r", 5)
	if err != nil {
		t.Fatalf("ChangedFileStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("stats = %v, want 2 entries", stats)
	}
	if stats[0].Path != "a.go" || stats[0].Additions != 1 || stats[0].Deletions != 1 {
		t.Errorf("stats[0] = %+v, want a.go +1/-1", stats[0])
	}
	if stats[1].Path != "b.go" || stats[1].Deletions != 1 {
		t.Errorf("stats[1] = %+v, want b.go with a deletion, falling back to old_path", stats[1])
	}
}

func TestHasWriteAccessChecksAccessLevel(t *testing.T) {
	for _, tt := range []struct {
		name        string
		accessLevel int
		want        bool
	}{
		{"owner", 50, true},
		{"maintainer", 40, true},
		{"developer", 30, true},
		{"reporter", 20, false},
		{"guest", 10, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/users"):
					_, _ = fmt.Fprint(w, `[{"id": 9}]`)
				default:
					_, _ = fmt.Fprintf(w, `{"access_level": %d}`, tt.accessLevel)
				}
			}))
			defer srv.Close()

			c, _ := newTestClient(t, srv)
			ok, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny")
			if err != nil {
				t.Fatalf("HasWriteAccess: %v", err)
			}
			if ok != tt.want {
				t.Errorf("HasWriteAccess = %v, want %v", ok, tt.want)
			}
		})
	}
}

func TestHasWriteAccessNoSuchUserIsFalseNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	ok, err := c.HasWriteAccess(context.Background(), "o", "r", "ghost")
	if err != nil {
		t.Fatalf("HasWriteAccess: %v", err)
	}
	if ok {
		t.Error("HasWriteAccess = true, want false for an unknown user")
	}
}

func TestHasWriteAccessNonMemberIsFalseNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users") {
			_, _ = fmt.Fprint(w, `[{"id": 9}]`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message": "404 Not found"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	ok, err := c.HasWriteAccess(context.Background(), "o", "r", "outsider")
	if err != nil {
		t.Fatalf("HasWriteAccess: %v", err)
	}
	if ok {
		t.Error("HasWriteAccess = true, want false for a non-member")
	}
}

func TestHasWriteAccessRejectsEmptyLogin(t *testing.T) {
	c, _ := New(testToken)
	if _, err := c.HasWriteAccess(context.Background(), "o", "r", ""); err == nil {
		t.Error("HasWriteAccess with empty login: want error, got nil")
	}
}

func TestHasWriteAccessPropagatesLookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, WithMaxRetries(0))
	if _, err := c.HasWriteAccess(context.Background(), "o", "r", "evgeny"); !errors.Is(err, ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
}

func boolPtr(b bool) *bool { return &b }

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

var _ host.PermissionChecker = (*Client)(nil)
