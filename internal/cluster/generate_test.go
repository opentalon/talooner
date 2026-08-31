package cluster

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

func TestGenerateRulesetReturnsVerifiedPair(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionGenerateRuleset: structured(t, &taloonerpb.GenerateRulesetResponse{
			Ruleset:     `rule "x" {}`,
			RulesetTest: `test "y" {}`,
			Source:      "llm",
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	resp, err := c.GenerateRuleset(t.Context(), "a Go CLI")
	if err != nil {
		t.Fatalf("GenerateRuleset: %v", err)
	}
	if resp.GetSource() != "llm" {
		t.Errorf("source = %q, want llm", resp.GetSource())
	}
	if resp.GetRuleset() != `rule "x" {}` {
		t.Errorf("ruleset mismatch: %q", resp.GetRuleset())
	}

	if got := callFor(t, f, ActionGenerateRuleset).GetArgs()["repo_summary"]; !strings.Contains(got, "Go CLI") {
		t.Errorf("repo_summary arg = %q", got)
	}
}

func TestGenerateRulesetReportsFallback(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionGenerateRuleset: structured(t, &taloonerpb.GenerateRulesetResponse{
			Source: "fallback",
			Note:   "no host to perform the call",
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	resp, err := c.GenerateRuleset(t.Context(), "a Go CLI")
	if err != nil {
		t.Fatalf("GenerateRuleset: %v", err)
	}
	if resp.GetSource() != "fallback" {
		t.Errorf("source = %q, want fallback", resp.GetSource())
	}
	if resp.GetRuleset() != "" || resp.GetRulesetTest() != "" {
		t.Error("fallback response should carry no ruleset text")
	}
}

// An empty summary is rejected here rather than at the cluster, same
// convention as ValidateRuleset's empty-ruleset guard.
func TestGenerateRulesetRejectsAnEmptySummary(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := c.GenerateRuleset(t.Context(), "  \n"); err == nil {
		t.Fatal("GenerateRuleset = nil, want an error")
	}
	for _, call := range f.calls {
		if call.GetAction() == ActionGenerateRuleset {
			t.Error("an empty repo summary reached the cluster")
		}
	}
}
