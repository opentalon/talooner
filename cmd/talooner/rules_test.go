package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/credentials"
)

// withWhoami routes the whoami handshake Dial always performs to a fixed
// success, and everything else to respond — cluster.Dial never returns a
// client that hasn't handshaken, so any test dialling a fake plugin needs
// both.
func withWhoami(t *testing.T, respond func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	t.Helper()
	whoami := whoamiOK(t)
	return func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		if req.GetAction() == cluster.ActionWhoami {
			return whoami(req)
		}
		return respond(req)
	}
}

func writeRuleset(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.tln"), []byte(body), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func seedRulesCreds(t *testing.T, host string) {
	t.Helper()
	path, err := credentials.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if err := credentials.Save(path, credentials.Credentials{Host: host, APIKey: testKey}); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestRulesValidateRequiresPathArg(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate"}, &out, &errw)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestRulesValidateMissingFile(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", filepath.Join(dir, "nope")}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "rules.tln") {
		t.Errorf("stderr = %q, want it to name the ruleset file", errw.String())
	}
}

func TestRulesValidateNoStoredCredentials(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at `cluster login`", errw.String())
	}
}

func TestRulesValidateValid(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.ValidateRulesetResponse{Valid: true}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "is valid") {
		t.Errorf("stdout = %q, want it to say the ruleset is valid", out.String())
	}
	if errw.Len() != 0 {
		t.Errorf("stderr = %q, want empty on a valid ruleset", errw.String())
	}
}

func TestRulesValidateInvalidPrintsDiagnostics(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.ValidateRulesetResponse{
		Valid: false,
		Diagnostics: []*taloonerpb.Diagnostic{
			{
				Severity: taloonerpb.Severity_SEVERITY_ERROR,
				Message:  `unknown verb "aprove", want one of: approve, block, comment, assign, require, emit`,
				Line:     3,
				Column:   8,
			},
		},
	}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do aprove }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "aprove") {
		t.Errorf("stderr = %q, want it to name the bad verb", errw.String())
	}
	if !strings.Contains(errw.String(), "approve, block") {
		t.Errorf("stderr = %q, want it to list the valid verbs", errw.String())
	}
	if !strings.Contains(errw.String(), "rules.tln:3:8") {
		t.Errorf("stderr = %q, want a path:line:column position", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on an invalid ruleset", out.String())
	}
}

func writeRulesetTest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "rules.tln.test"), []byte(body), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestRulesTestRequiresPathArg(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test"}, &out, &errw)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestRulesTestMissingRulesetFile(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "rules.tln") {
		t.Errorf("stderr = %q, want it to name the ruleset file", errw.String())
	}
}

func TestRulesTestMissingTestFile(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "rules.tln.test") {
		t.Errorf("stderr = %q, want it to name the test file", errw.String())
	}
}

func TestRulesTestNoStoredCredentials(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	writeRulesetTest(t, dir, "test \"y\" { }\n")
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at `cluster login`", errw.String())
	}
}

func TestRulesTestAllPassing(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.RunRulesetTestResponse{
		Results: []*taloonerpb.TestOutcome{
			{Name: "approves a clean pr", Passed: true},
		},
	}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	writeRulesetTest(t, dir, "test \"y\" { }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "PASS approves a clean pr") {
		t.Errorf("stdout = %q, want the passing test named", out.String())
	}
	if !strings.Contains(out.String(), "1/1 tests passed") {
		t.Errorf("stdout = %q, want a summary line", out.String())
	}
	if errw.Len() != 0 {
		t.Errorf("stderr = %q, want empty when every test passes", errw.String())
	}
}

func TestRulesTestSomeFailing(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.RunRulesetTestResponse{
		Results: []*taloonerpb.TestOutcome{
			{Name: "approves a clean pr", Passed: true},
			{Name: "blocks unresolved conflicts", Passed: false, Errors: []string{"expected did approve, got did nothing"}},
		},
	}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	writeRulesetTest(t, dir, "test \"y\" { }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "FAIL blocks unresolved conflicts") {
		t.Errorf("stdout = %q, want the failing test named", out.String())
	}
	if !strings.Contains(out.String(), "expected did approve, got did nothing") {
		t.Errorf("stdout = %q, want the assertion error", out.String())
	}
	if !strings.Contains(errw.String(), "1/2 tests failed") {
		t.Errorf("stderr = %q, want a failure summary", errw.String())
	}
}

func TestRulesTestCompileFailurePrintsDiagnostics(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.RunRulesetTestResponse{
		Diagnostics: []*taloonerpb.Diagnostic{{
			Severity: taloonerpb.Severity_SEVERITY_ERROR,
			Message:  `unexpected token "do"`,
			Line:     3,
			Column:   9,
		}},
	}))})
	seedRulesCreds(t, host)
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do }\n")
	writeRulesetTest(t, dir, "test \"y\" { }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "line 3:9") {
		t.Errorf("stderr = %q, want a line:column position", errw.String())
	}
	if !strings.Contains(errw.String(), `unexpected token "do"`) {
		t.Errorf("stderr = %q, want the diagnostic message", errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a compile failure", out.String())
	}
}

func TestRulesTestClusterUnreachable(t *testing.T) {
	withHome(t)
	seedRulesCreds(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")
	writeRulesetTest(t, dir, "test \"y\" { }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "test", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cannot reach cluster") {
		t.Errorf("stderr = %q, want the unreachable-host message", errw.String())
	}
}

func TestRulesValidateClusterUnreachable(t *testing.T) {
	withHome(t)
	seedRulesCreds(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	writeRuleset(t, dir, "rule \"x\" { do approve }\n")

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "validate", dir}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cannot reach cluster") {
		t.Errorf("stderr = %q, want the unreachable-host message", errw.String())
	}
}

// serveGitHubForPlan serves just enough of the GitHub API for run.Runner.Plan
// to extract facts.PR's inputs from a fixture PR: no fork, no config/modules/
// teams/CODEOWNERS, one changed file, no checks, no reviews. It records every
// method it sees, so a test can assert Plan never issues a write.
func serveGitHubForPlan(t *testing.T) (url string, methods func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/.github/talooner/rules.tln"):
			body := "rule \"x\" { do approve }\n"
			_, _ = fmt.Fprintf(w, `{"type":"file","size":%d,"encoding":"base64","content":%q}`,
				len(body), base64.StdEncoding.EncodeToString([]byte(body)))
		case strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = fmt.Fprint(w, `{"state":"success","total_count":0,"statuses":[]}`)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
		default: // the pull request itself
			_, _ = fmt.Fprint(w, `{
				"number": 42,
				"head": {"sha": "abc123", "ref": "feat/x", "repo": {"full_name": "opentalon/talooner"}},
				"base": {"sha": "def456", "ref": "master", "repo": {"full_name": "opentalon/talooner"}},
				"user": {"login": "evgeny"},
				"title": "Add a thing", "body": "", "state": "open", "mergeable": true,
				"additions": 10, "deletions": 3, "changed_files": 1, "commits": 2,
				"assignees": [], "requested_reviewers": [], "requested_teams": []
			}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func writeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func TestRulesPlanRequiresRepoAndPR(t *testing.T) {
	withHome(t)
	cases := [][]string{
		{"rules", "plan"},
		{"rules", "plan", "--repo", "opentalon/talooner"},
		{"rules", "plan", "--pr", "42"},
		{"rules", "plan", "--repo", "not-owner-slash-name", "--pr", "42"},
	}
	for _, args := range cases {
		var out, errw bytes.Buffer
		code := run(context.Background(), args, &out, &errw)
		if code != 2 {
			t.Errorf("run(%v) code = %d, want 2", args, code)
		}
	}
}

func TestRulesPlanNoStoredCredentials(t *testing.T) {
	withHome(t)
	t.Setenv("GITHUB_TOKEN", "ghs_test")
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "plan", "--repo", "opentalon/talooner", "--pr", "42"}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at `cluster login`", errw.String())
	}
}

func TestRulesPlanRendersActionsAndWritesNothing(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: withWhoami(t, structured(t, &taloonerpb.EvaluatePrResponse{
		Plan: []*taloonerpb.Action{{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"}},
	}))})
	seedRulesCreds(t, host)

	ghURL, methods := serveGitHubForPlan(t)
	t.Setenv("GITHUB_TOKEN", "ghs_test")
	t.Setenv("GITHUB_API_URL", ghURL)

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "plan", "--repo", "opentalon/talooner", "--pr", "42"}, &out, &errw)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "approve") {
		t.Errorf("stdout = %q, want the planned approve action", out.String())
	}
	for _, call := range methods() {
		if writeMethod(strings.SplitN(call, " ", 2)[0]) {
			t.Errorf("GitHub saw %q; rules plan must never write", call)
		}
	}
}

func TestRulesPlanNoGitHubToken(t *testing.T) {
	withHome(t)
	host := serve(t, &fakePlugin{respond: whoamiOK(t)})
	seedRulesCreds(t, host)

	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"rules", "plan", "--repo", "opentalon/talooner", "--pr", "42"}, &out, &errw)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "GITHUB_TOKEN") {
		t.Errorf("stderr = %q, want it to name the missing token", errw.String())
	}
}
