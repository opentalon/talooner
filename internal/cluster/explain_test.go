package cluster

import (
	"errors"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

func TestExplainPRArgs(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionExplainPR: structured(t, &taloonerpb.ExplainPrResponse{
			Explain: &taloonerpb.Explain{
				Summary: "blocked: missing description",
				Firings: []*taloonerpb.RuleFiring{{Rule: "needs description", Priority: "HIGH"}},
			},
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	resp, err := c.ExplainPR(t.Context(), "opentalon/talooner", 42, "abc123")
	if err != nil {
		t.Fatalf("ExplainPR: %v", err)
	}
	if resp.GetExplain().GetSummary() != "blocked: missing description" {
		t.Errorf("summary = %q", resp.GetExplain().GetSummary())
	}
	if got := resp.GetExplain().GetFirings(); len(got) != 1 || got[0].GetRule() != "needs description" {
		t.Errorf("firings = %v", got)
	}

	args := callFor(t, f, ActionExplainPR).GetArgs()
	for k, want := range map[string]string{"repo": "opentalon/talooner", "pr": "42", "head_sha": "abc123"} {
		if args[k] != want {
			t.Errorf("arg %s = %q, want %q", k, args[k], want)
		}
	}
}

// A sha nothing was ever evaluated at is the plugin's own distinct refusal,
// not an empty explanation that would read like "no rules fired".
func TestExplainPRRefusalIsErrAction(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionExplainPR: failWith("talooner: no decision recorded for o/r#1 at abc123; it was not evaluated at that sha"),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_, err = c.ExplainPR(t.Context(), "o/r", 1, "abc123")
	if !errors.Is(err, ErrAction) {
		t.Fatalf("err = %v, want ErrAction", err)
	}
}

func TestExplainPRRejectsMissingScope(t *testing.T) {
	f := scripted(t, nil)
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	for _, tt := range []struct {
		name    string
		repo    string
		pr      int
		headSHA string
	}{
		{"no repo", "", 1, "abc"},
		{"no pr", "o/r", 0, "abc"},
		{"no head sha", "o/r", 1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.ExplainPR(t.Context(), tt.repo, tt.pr, tt.headSHA); err == nil {
				t.Fatal("ExplainPR = nil error")
			}
		})
	}
	if len(f.calls) != 1 {
		t.Errorf("made %d calls, want only the handshake", len(f.calls))
	}
}
