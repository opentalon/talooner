package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/onboard"
)

type gitlabCall struct {
	method string
	path   string
	body   map[string]any
}

// fakeGitLabServer stands in for GitLab's REST API for init/onboard's
// --host gitlab path — records every variables/merge_requests call it sees
// so a test can assert exactly what was sent, the way fakeGH/fakeGit do for
// the GitHub path.
func fakeGitLabServer(t *testing.T) (string, func() []gitlabCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []gitlabCall

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, gitlabCall{method: r.Method, path: r.URL.Path, body: body})

		switch {
		case strings.Contains(r.URL.Path, "/variables/") && r.Method == http.MethodPut:
			// Every variable in these tests is created fresh — PUT never
			// finds an existing one, so onboard.go's upsert always falls
			// back to POST.
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"404 Variable Not Found"}`)
		case strings.HasSuffix(r.URL.Path, "/variables") && r.Method == http.MethodPost:
			_, _ = fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests") && r.Method == http.MethodPost:
			_, _ = fmt.Fprint(w, `{"web_url":"https://gitlab.example.com/acme/api/-/merge_requests/1"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"unexpected call"}`)
		}
	}))
	t.Cleanup(srv.Close)

	return srv.URL, func() []gitlabCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]gitlabCall(nil), calls...)
	}
}

func withGitLabEnv(t *testing.T, apiURL string) {
	t.Helper()
	t.Setenv("GITLAB_TOKEN", "glpat-test-token")
	t.Setenv("CI_API_V4_URL", apiURL)
}

func TestInitGitLabHappyPathSetsProjectVariables(t *testing.T) {
	creds := seedCreds(t)
	t.Chdir(t.TempDir())
	apiURL, calls := fakeGitLabServer(t)
	withGitLabEnv(t, apiURL)

	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api", "--host", "gitlab"}, &out, &errw, &fakeGH{})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if strings.Contains(out.String(), creds.APIKey) || strings.Contains(errw.String(), creds.APIKey) {
		t.Error("API key leaked into stdout/stderr")
	}

	got := calls()
	var posted []gitlabCall
	for _, c := range got {
		if c.method == http.MethodPost {
			posted = append(posted, c)
		}
	}
	if len(posted) != 2 {
		t.Fatalf("posted variable calls = %v, want 2", posted)
	}
	byKey := map[string]gitlabCall{}
	for _, c := range posted {
		byKey[fmt.Sprint(c.body["key"])] = c
	}
	host, ok := byKey["OPENTALON_HOST"]
	if !ok {
		t.Fatal("no OPENTALON_HOST variable was set")
	}
	if host.body["value"] != creds.Host || host.body["masked"] != false {
		t.Errorf("OPENTALON_HOST call = %+v, want unmasked with the stored host", host.body)
	}
	apiKey, ok := byKey["OPENTALON_API_KEY"]
	if !ok {
		t.Fatal("no OPENTALON_API_KEY variable was set")
	}
	if apiKey.body["value"] != creds.APIKey || apiKey.body["masked"] != true {
		t.Errorf("OPENTALON_API_KEY call = %+v, want masked with the stored key", apiKey.body)
	}
	for _, c := range posted {
		if !strings.Contains(c.path, "/projects/") || strings.Contains(c.path, "/groups/") {
			t.Errorf("call path = %q, want a /projects/ path (no --org given)", c.path)
		}
	}
}

func TestInitGitLabOrgFlagSetsGroupVariables(t *testing.T) {
	seedCreds(t)
	t.Chdir(t.TempDir())
	apiURL, calls := fakeGitLabServer(t)
	withGitLabEnv(t, apiURL)

	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api", "--host", "gitlab", "--org", "acme"}, &out, &errw, &fakeGH{})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	for _, c := range calls() {
		if !strings.Contains(c.path, "/groups/") {
			t.Errorf("call path = %q, want a /groups/ path with --org set", c.path)
		}
	}
}

func TestInitGitLabMissingTokenReportsWhichEnvVar(t *testing.T) {
	seedCreds(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_TOKEN", "")

	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api", "--host", "gitlab"}, &out, &errw, &fakeGH{})
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "GITLAB_TOKEN") {
		t.Errorf("stderr = %q, want it to name GITLAB_TOKEN", errw.String())
	}
}

func TestInitRejectsUnknownHost(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := runInit(context.Background(), []string{"--repo", "acme/api", "--host", "bitbucket"}, &out, &errw, &fakeGH{})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "--host") {
		t.Errorf("stderr = %q, want it to name --host", errw.String())
	}
}

func TestOnboardGitLabHappyPathWritesCIFileAndOpensMR(t *testing.T) {
	withHome(t)
	f := routedOnboardFake(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		"generate_ruleset": structured(t, &taloonerpb.GenerateRulesetResponse{
			Ruleset:     `rule "x" {}`,
			RulesetTest: `test "y" {}`,
			Source:      "llm",
		}),
		"validate_ruleset": passingValidate(t),
		"run_ruleset_test": passingTest(t),
	})
	clusterHost := serve(t, f)
	seedRulesCreds(t, clusterHost)
	t.Chdir(t.TempDir())

	apiURL, calls := fakeGitLabServer(t)
	withGitLabEnv(t, apiURL)

	gh := &fakeGH{}
	git := &fakeGit{fail: map[string]error{
		"show-ref --verify --quiet refs/heads/talooner-onboarding": errors.New("not a valid ref"),
	}}
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api", "--host", "gitlab"}, &out, &errw, gh, git)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}

	if _, err := os.Stat(onboard.WorkflowPath); !os.IsNotExist(err) {
		t.Errorf("expected %s NOT to exist on the gitlab path, err=%v", onboard.WorkflowPath, err)
	}
	ciBytes, err := os.ReadFile(onboard.GitLabCIPath)
	if err != nil {
		t.Fatalf("reading %s: %v", onboard.GitLabCIPath, err)
	}
	if !bytes.Equal(ciBytes, onboard.GitLabCI) {
		t.Error("gitlab-ci file = not the starter template")
	}

	for _, call := range gh.calls {
		t.Errorf("unexpected gh call on the gitlab path: %v", call)
	}

	foundCommit := false
	for _, call := range git.calls {
		if call[0] == "add" {
			if containsArg(call, onboard.WorkflowPath) {
				t.Errorf("git add args = %v, must not stage the GitHub workflow path", call)
			}
			if !containsArg(call, onboard.GitLabCIPath) {
				t.Errorf("git add args = %v, want it to stage %s", call, onboard.GitLabCIPath)
			}
			foundCommit = true
		}
	}
	if !foundCommit {
		t.Error("no git add call found")
	}

	foundMR := false
	for _, c := range calls() {
		if strings.HasSuffix(c.path, "/merge_requests") {
			foundMR = true
			if c.body["source_branch"] != defaultOnboardBranch {
				t.Errorf("mr source_branch = %v, want %s", c.body["source_branch"], defaultOnboardBranch)
			}
			if c.body["title"] != "talooner onboarding" {
				t.Errorf("mr title = %v, want \"talooner onboarding\"", c.body["title"])
			}
		}
	}
	if !foundMR {
		t.Error("no merge_requests call found")
	}
	if !strings.Contains(out.String(), "merge_requests/1") {
		t.Errorf("stdout = %q, want it to print the opened MR url", out.String())
	}
}

func TestOnboardGitLabMissingTokenReportsWhichEnvVar(t *testing.T) {
	withHome(t)
	f := routedOnboardFake(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		"generate_ruleset": structured(t, &taloonerpb.GenerateRulesetResponse{
			Ruleset: `rule "x" {}`, RulesetTest: `test "y" {}`, Source: "llm",
		}),
		"validate_ruleset": passingValidate(t),
		"run_ruleset_test": passingTest(t),
	})
	clusterHost := serve(t, f)
	seedRulesCreds(t, clusterHost)
	t.Chdir(t.TempDir())
	t.Setenv("GITLAB_TOKEN", "")

	git := &fakeGit{fail: map[string]error{
		"show-ref --verify --quiet refs/heads/talooner-onboarding": errors.New("not a valid ref"),
	}}
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api", "--host", "gitlab"}, &out, &errw, &fakeGH{}, git)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "GITLAB_TOKEN") {
		t.Errorf("stderr = %q, want it to name GITLAB_TOKEN", errw.String())
	}
}
