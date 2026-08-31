package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/onboard"
)

// fakeGit scripts git responses the same way fakeGH scripts gh ones.
type fakeGit struct {
	fail  map[string]error
	calls [][]string
}

func (f *fakeGit) Run(_ context.Context, _ string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) > 0 {
		if err, ok := f.fail[args[0]]; ok {
			return "", err
		}
	}
	return "opened https://github.com/acme/api/pull/42\n", nil
}

// routedOnboardFake answers whoami with a healthy handshake and everything
// else from byAction, so a single fake server can stand in for
// generate_ruleset + validate_ruleset + run_ruleset_test in one test.
func routedOnboardFake(t *testing.T, byAction map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse) *fakePlugin {
	t.Helper()
	whoami := whoamiOK(t)
	return &fakePlugin{respond: func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		if req.GetAction() == "whoami" {
			return whoami(req)
		}
		if fn, ok := byAction[req.GetAction()]; ok {
			return fn(req)
		}
		return &pluginpb.ToolResultResponse{CallId: req.GetId(), Error: "unscripted action " + req.GetAction()}
	}}
}

func passingValidate(t *testing.T) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	return structured(t, &taloonerpb.ValidateRulesetResponse{Valid: true})
}

func passingTest(t *testing.T) func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
	return structured(t, &taloonerpb.RunRulesetTestResponse{
		Results: []*taloonerpb.TestOutcome{{Name: "a test", Passed: true}},
	})
}

func TestOnboardRequiresRepoFlag(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), nil, &out, &errw, &fakeGH{}, &fakeGit{})
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "--repo") {
		t.Errorf("stderr = %q, want it to name --repo", errw.String())
	}
}

func TestOnboardNoStoredCredentials(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, &fakeGH{}, &fakeGit{})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "cluster login") {
		t.Errorf("stderr = %q, want it to point at cluster login", errw.String())
	}
}

func TestOnboardLLMPathHappyPath(t *testing.T) {
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
	host := serve(t, f)
	seedRulesCreds(t, host)
	t.Chdir(t.TempDir())

	gh, git := &fakeGH{}, &fakeGit{}
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, gh, git)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}

	rulesetBytes, err := os.ReadFile(onboard.RulesetPath)
	if err != nil {
		t.Fatalf("reading %s: %v", onboard.RulesetPath, err)
	}
	if string(rulesetBytes) != `rule "x" {}` {
		t.Errorf("ruleset = %q, want the generated one", rulesetBytes)
	}

	if len(git.calls) != 4 {
		t.Fatalf("git calls = %v, want 4 (checkout, add, commit, push)", git.calls)
	}
	if git.calls[0][0] != "checkout" {
		t.Errorf("first git call = %v, want checkout", git.calls[0])
	}
	if git.calls[3][0] != "push" {
		t.Errorf("last git call = %v, want push", git.calls[3])
	}

	foundPRCreate := false
	for _, call := range gh.calls {
		if len(call) > 1 && call[0] == "pr" && call[1] == "create" {
			foundPRCreate = true
			if !containsArg(call, "talooner onboarding") {
				t.Errorf("pr create args = %v, want title \"talooner onboarding\"", call)
			}
		}
	}
	if !foundPRCreate {
		t.Errorf("gh calls = %v, want a pr create call", gh.calls)
	}
}

func TestOnboardFallbackPathUsesStarterRuleset(t *testing.T) {
	withHome(t)
	f := routedOnboardFake(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		"generate_ruleset": structured(t, &taloonerpb.GenerateRulesetResponse{
			Source: "fallback",
			Note:   "no host to perform the call",
		}),
		"validate_ruleset": passingValidate(t),
		"run_ruleset_test": passingTest(t),
	})
	host := serve(t, f)
	seedRulesCreds(t, host)
	t.Chdir(t.TempDir())

	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api", "--no-pr"}, &out, &errw, &fakeGH{}, &fakeGit{})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}

	got, err := os.ReadFile(onboard.RulesetPath)
	if err != nil {
		t.Fatalf("reading %s: %v", onboard.RulesetPath, err)
	}
	if string(got) != string(onboard.Ruleset) {
		t.Error("fallback should write onboard's own starter ruleset")
	}
	if !strings.Contains(out.String(), "fell back") {
		t.Errorf("stdout = %q, want it to say generate_ruleset fell back", out.String())
	}
}

func TestOnboardValidateFailureStopsBeforeGit(t *testing.T) {
	withHome(t)
	f := routedOnboardFake(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		"generate_ruleset": structured(t, &taloonerpb.GenerateRulesetResponse{
			Ruleset: `rule "x" { do }`, RulesetTest: `test "y" {}`, Source: "llm",
		}),
		"validate_ruleset": structured(t, &taloonerpb.ValidateRulesetResponse{
			Valid: false,
			Diagnostics: []*taloonerpb.Diagnostic{{
				Severity: taloonerpb.Severity_SEVERITY_ERROR, Message: "unexpected token",
			}},
		}),
	})
	host := serve(t, f)
	seedRulesCreds(t, host)
	t.Chdir(t.TempDir())

	gh, git := &fakeGH{}, &fakeGit{}
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, gh, git)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if len(git.calls) != 0 {
		t.Errorf("git calls = %v, want none — a failed verification must not reach git", git.calls)
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh calls = %v, want none", gh.calls)
	}
}

func TestOnboardTestFailureStopsBeforeGit(t *testing.T) {
	withHome(t)
	f := routedOnboardFake(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		"generate_ruleset": structured(t, &taloonerpb.GenerateRulesetResponse{
			Ruleset: `rule "x" {}`, RulesetTest: `test "y" {}`, Source: "llm",
		}),
		"validate_ruleset": passingValidate(t),
		"run_ruleset_test": structured(t, &taloonerpb.RunRulesetTestResponse{
			Results: []*taloonerpb.TestOutcome{{Name: "a test", Passed: false, Errors: []string{"boom"}}},
		}),
	})
	host := serve(t, f)
	seedRulesCreds(t, host)
	t.Chdir(t.TempDir())

	git := &fakeGit{}
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api"}, &out, &errw, &fakeGH{}, git)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if len(git.calls) != 0 {
		t.Errorf("git calls = %v, want none — a failing test must not reach git", git.calls)
	}
}

func TestOnboardNoPRSkipsGitAndGH(t *testing.T) {
	withHome(t)
	f := routedOnboardFake(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		"generate_ruleset": structured(t, &taloonerpb.GenerateRulesetResponse{
			Ruleset: `rule "x" {}`, RulesetTest: `test "y" {}`, Source: "llm",
		}),
		"validate_ruleset": passingValidate(t),
		"run_ruleset_test": passingTest(t),
	})
	host := serve(t, f)
	seedRulesCreds(t, host)
	t.Chdir(t.TempDir())

	gh, git := &fakeGH{}, &fakeGit{}
	var out, errw bytes.Buffer
	code := runOnboard(context.Background(), []string{"--repo", "acme/api", "--no-pr"}, &out, &errw, gh, git)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errw.String())
	}
	if len(git.calls) != 0 || len(gh.calls) != 0 {
		t.Errorf("--no-pr should skip git/gh entirely, got git=%v gh=%v", git.calls, gh.calls)
	}
	if !strings.Contains(out.String(), "--no-pr") {
		t.Errorf("stdout = %q, want it to say --no-pr skipped the PR", out.String())
	}
}
