package cluster

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

func TestRunRulesetTestReturnsOutcomes(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionRunRulesetTest: structured(t, &taloonerpb.RunRulesetTestResponse{
			Results: []*taloonerpb.TestOutcome{
				{Name: "approves a clean pr", Passed: true},
				{Name: "blocks unresolved conflicts", Passed: false, Errors: []string{"expected did approve, got did nothing"}},
			},
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	resp, err := c.RunRulesetTest(t.Context(), "rule \"x\" { do }\n", "test \"y\" { }\n")
	if err != nil {
		t.Fatalf("RunRulesetTest: %v", err)
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("results = %v", resp.GetResults())
	}
	if resp.GetResults()[0].GetPassed() != true {
		t.Errorf("results[0].passed = false, want true")
	}
	if resp.GetResults()[1].GetPassed() {
		t.Errorf("results[1].passed = true, want false")
	}

	args := callFor(t, f, ActionRunRulesetTest).GetArgs()
	if !strings.Contains(args["ruleset"], "do") {
		t.Errorf("ruleset arg = %q", args["ruleset"])
	}
	if !strings.Contains(args["test_source"], "test") {
		t.Errorf("test_source arg = %q", args["test_source"])
	}
}

func TestRunRulesetTestReturnsDiagnosticsOnCompileFailure(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionRunRulesetTest: structured(t, &taloonerpb.RunRulesetTestResponse{
			Diagnostics: []*taloonerpb.Diagnostic{{
				Severity: taloonerpb.Severity_SEVERITY_ERROR,
				Message:  `unexpected token "do"`,
				Line:     3,
				Column:   9,
			}},
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	resp, err := c.RunRulesetTest(t.Context(), "rule \"x\" { do }\n", "test \"y\" { }\n")
	if err != nil {
		t.Fatalf("RunRulesetTest: %v", err)
	}
	if len(resp.GetResults()) != 0 {
		t.Errorf("results = %v, want none on compile failure", resp.GetResults())
	}
	if len(resp.GetDiagnostics()) != 1 {
		t.Fatalf("diagnostics = %v", resp.GetDiagnostics())
	}
}

// An empty ruleset or test source is rejected here rather than at the
// cluster: the caller reaching this with nothing to test is a bug worth
// naming locally.
func TestRunRulesetTestRejectsEmptyInputs(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	if _, err := c.RunRulesetTest(t.Context(), "  \n", "test \"y\" { }\n"); err == nil {
		t.Fatal("RunRulesetTest with empty ruleset = nil, want an error")
	}
	if _, err := c.RunRulesetTest(t.Context(), "rule \"x\" { do }\n", "  \n"); err == nil {
		t.Fatal("RunRulesetTest with empty test source = nil, want an error")
	}
	for _, call := range f.calls {
		if call.GetAction() == ActionRunRulesetTest {
			t.Error("an empty input reached the cluster")
		}
	}
}
