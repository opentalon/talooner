package cluster

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

func TestValidateRulesetReturnsPositionedDiagnostics(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionValidateRuleset: structured(t, &taloonerpb.ValidateRulesetResponse{
			Valid: false,
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

	resp, err := c.ValidateRuleset(t.Context(), "rule \"x\" { do }\n")
	if err != nil {
		t.Fatalf("ValidateRuleset: %v", err)
	}
	if resp.GetValid() {
		t.Error("valid = true, want false")
	}
	if len(resp.GetDiagnostics()) != 1 {
		t.Fatalf("diagnostics = %v", resp.GetDiagnostics())
	}
	// The position is the whole reason for the call: a check-run annotation has
	// to name a line.
	d := resp.GetDiagnostics()[0]
	if d.GetLine() != 3 || d.GetColumn() != 9 {
		t.Errorf("position = %d:%d, want 3:9", d.GetLine(), d.GetColumn())
	}

	if got := callFor(t, f, ActionValidateRuleset).GetArgs()["ruleset"]; !strings.Contains(got, "do") {
		t.Errorf("ruleset arg = %q", got)
	}
}

// An empty ruleset is rejected here rather than at the cluster: the caller
// reaching this with nothing to validate is a bug worth naming locally.
func TestValidateRulesetRejectsAnEmptyRuleset(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := c.ValidateRuleset(t.Context(), "  \n"); err == nil {
		t.Fatal("ValidateRuleset = nil, want an error")
	}
	for _, call := range f.calls {
		if call.GetAction() == ActionValidateRuleset {
			t.Error("an empty ruleset reached the cluster")
		}
	}
}
