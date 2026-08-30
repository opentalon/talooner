package cluster

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// scripted answers whoami with a healthy handshake and every other action from
// the map, so one fake serves a dial plus the call under test.
func scripted(t *testing.T, byAction map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse) *fakePlugin {
	t.Helper()
	whoami := whoamiOK(t)
	return &fakePlugin{respond: func(req *pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse {
		if req.GetAction() == ActionWhoami {
			return whoami(req)
		}
		respond, ok := byAction[req.GetAction()]
		if !ok {
			t.Errorf("unexpected action %q", req.GetAction())
			return &pluginpb.ToolResultResponse{CallId: req.GetId(), Error: "unexpected action"}
		}
		return respond(req)
	}}
}

// callFor returns the recorded request for an action, or fails.
func callFor(t *testing.T, f *fakePlugin, action string) *pluginpb.ToolCallRequest {
	t.Helper()
	for _, c := range f.calls {
		if c.GetAction() == action {
			return c
		}
	}
	t.Fatalf("no %s call was made", action)
	return nil
}

func TestEvaluatePRArgs(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionEvaluatePR: structured(t, &taloonerpb.EvaluatePrResponse{
			Actions: []*taloonerpb.Action{{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"}},
			Explain: &taloonerpb.Explain{Summary: "all good"},
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	resp, err := c.EvaluatePR(t.Context(), EvaluateRequest{
		Repo:    "opentalon/talooner",
		PR:      42,
		HeadSHA: "abc123",
		Facts:   map[string]any{"pr.number": 42, "pr.changed_files": []string{"a.go"}},
		Ruleset: "rule \"x\" { }",
	})
	if err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}
	if len(resp.GetActions()) != 1 || resp.GetActions()[0].GetVerb() != taloonerpb.Verb_VERB_APPROVE {
		t.Errorf("actions = %v", resp.GetActions())
	}
	if resp.GetExplain().GetSummary() != "all good" {
		t.Errorf("summary = %q", resp.GetExplain().GetSummary())
	}

	args := callFor(t, f, ActionEvaluatePR).GetArgs()
	for k, want := range map[string]string{
		"repo": "opentalon/talooner", "pr": "42", "head_sha": "abc123",
		"ruleset": "rule \"x\" { }", "mode": "execute", "force": "false",
	} {
		if args[k] != want {
			t.Errorf("arg %s = %q, want %q", k, args[k], want)
		}
	}
	if args[ArgAPIKey] != testKey {
		t.Error("evaluate_pr went out without the api key")
	}

	// The facts arg is the flat dotted map the plugin's decoder takes; anything
	// that is not bool/int/string/[]string is a hard decode error over there.
	var set map[string]any
	if err := json.Unmarshal([]byte(args["facts"]), &set); err != nil {
		t.Fatalf("facts arg is not JSON: %v", err)
	}
	if set["pr.number"] != float64(42) {
		t.Errorf("facts[pr.number] = %v", set["pr.number"])
	}
}

// The code_units arg is wire-compatible with talooner-plugin's own codeUnit
// struct (internal/service/units.go) — not generated from a shared proto, so
// a field rename on either side is a silent break the plugin only reports as
// "invalid code_units" at runtime. This test is what would catch it here.
func TestEvaluatePRCodeUnitsArg(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionEvaluatePR: structured(t, &taloonerpb.EvaluatePrResponse{}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	_, err = c.EvaluatePR(t.Context(), EvaluateRequest{
		Repo:    "opentalon/talooner",
		PR:      42,
		HeadSHA: "abc123",
		Facts:   map[string]any{"pr.number": 42},
		Ruleset: "rule \"x\" { }",
		CodeUnits: []CodeUnit{
			{Name: "internal/auth", Important: true, DocURL: "docs/services/auth.md",
				DocContent: "auth must hash passwords", Diff: "- plaintext\n+ bcrypt", DiffTruncated: true},
		},
	})
	if err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}

	args := callFor(t, f, ActionEvaluatePR).GetArgs()
	var got []map[string]any
	if err := json.Unmarshal([]byte(args["code_units"]), &got); err != nil {
		t.Fatalf("code_units arg is not JSON: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("code_units = %v, want one unit", got)
	}
	want := map[string]any{
		"name": "internal/auth", "important": true, "doc_url": "docs/services/auth.md",
		"doc_content": "auth must hash passwords", "diff": "- plaintext\n+ bcrypt", "diff_truncated": true,
	}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("code_units[0][%q] = %v, want %v", k, got[0][k], v)
		}
	}
}

// No code units at all — most PRs touch nothing under a known layer — must
// not send a code_units arg, not an empty JSON list: units.go's parser treats
// the arg's absence and an empty string the same, but sending anything at all
// for the overwhelmingly common case is one string the plugin never needed.
func TestEvaluatePROmitsCodeUnitsWhenEmpty(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionEvaluatePR: structured(t, &taloonerpb.EvaluatePrResponse{}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	_, err = c.EvaluatePR(t.Context(), EvaluateRequest{
		Repo:    "opentalon/talooner",
		PR:      42,
		HeadSHA: "abc123",
		Facts:   map[string]any{"pr.number": 42},
		Ruleset: "rule \"x\" { }",
	})
	if err != nil {
		t.Fatalf("EvaluatePR: %v", err)
	}

	args := callFor(t, f, ActionEvaluatePR).GetArgs()
	if _, ok := args["code_units"]; ok {
		t.Errorf("code_units arg present = %q, want absent", args["code_units"])
	}
}

func TestEvaluatePRRejectsAnEmptyFactSet(t *testing.T) {
	f := scripted(t, nil)
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// The plugin re-derives from what arrives and drops every bot fact absent
	// from the request, so an empty set does not omit facts — it retracts them.
	_, err = c.EvaluatePR(t.Context(), EvaluateRequest{Repo: "o/r", PR: 1, HeadSHA: "abc"})
	if err == nil {
		t.Fatal("EvaluatePR = nil error for an empty fact set")
	}
	if len(f.calls) != 1 {
		t.Errorf("made %d calls, want only the handshake", len(f.calls))
	}
}

func TestEvaluatePRRejectsMissingScope(t *testing.T) {
	f := scripted(t, nil)
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	facts := map[string]any{"pr.number": 1}
	for _, tt := range []struct {
		name string
		req  EvaluateRequest
	}{
		{"no repo", EvaluateRequest{PR: 1, HeadSHA: "abc", Facts: facts}},
		{"no pr", EvaluateRequest{Repo: "o/r", HeadSHA: "abc", Facts: facts}},
		{"no head sha", EvaluateRequest{Repo: "o/r", PR: 1, Facts: facts}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.EvaluatePR(t.Context(), tt.req); err == nil {
				t.Fatal("EvaluatePR = nil error")
			}
		})
	}
}

// A plan is a dry run. The plugin keeps plan[] and actions[] distinct; if a
// response ever carries executable actions in plan mode, the call fails rather
// than handing them to an executor.
func TestEvaluatePRPlanModeRefusesExecutableActions(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionEvaluatePR: structured(t, &taloonerpb.EvaluatePrResponse{
			Actions: []*taloonerpb.Action{{Verb: taloonerpb.Verb_VERB_BLOCK, Target: "pr.merge"}},
		}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_, err = c.EvaluatePR(t.Context(), EvaluateRequest{
		Repo: "o/r", PR: 1, HeadSHA: "abc", Mode: ModePlan,
		Facts: map[string]any{"pr.number": 1},
	})
	if !errors.Is(err, ErrAction) {
		t.Fatalf("err = %v, want ErrAction", err)
	}
	if args := callFor(t, f, ActionEvaluatePR).GetArgs(); args["mode"] != "plan" {
		t.Errorf("mode = %q, want plan", args["mode"])
	}
}

func TestEvaluatePRRefusalIsErrAction(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionEvaluatePR: failWith("talooner: ruleset does not compile: line 3"),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_, err = c.EvaluatePR(t.Context(), EvaluateRequest{
		Repo: "o/r", PR: 1, HeadSHA: "abc", Facts: map[string]any{"pr.number": 1},
	})
	if !errors.Is(err, ErrAction) {
		t.Fatalf("err = %v, want ErrAction", err)
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v, want the plugin's own message kept", err)
	}
}

func TestSubscription(t *testing.T) {
	f := scripted(t, map[string]func(*pluginpb.ToolCallRequest) *pluginpb.ToolResultResponse{
		ActionIsSubscribed:    structured(t, &taloonerpb.IsSubscribedResponse{Subscribed: false}),
		ActionSetSubscription: structured(t, &taloonerpb.SetSubscriptionResponse{Subscribed: true}),
	})
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// A PR nobody ever invoked Talooner on is not subscribed, and that is an
	// answer rather than an error: the run exits 0 and the job shows as skipped.
	subscribed, err := c.IsSubscribed(t.Context(), "opentalon/talooner", 42)
	if err != nil || subscribed {
		t.Fatalf("IsSubscribed = %v, %v, want false, nil", subscribed, err)
	}

	state, err := c.SetSubscription(t.Context(), "opentalon/talooner", 42, true)
	if err != nil || !state {
		t.Fatalf("SetSubscription = %v, %v, want true, nil", state, err)
	}
	args := callFor(t, f, ActionSetSubscription).GetArgs()
	if args["repo"] != "opentalon/talooner" || args["pr"] != "42" || args["state"] != "true" {
		t.Errorf("set_subscription args = %v", args)
	}
}

func TestSubscriptionRejectsMissingScope(t *testing.T) {
	f := scripted(t, nil)
	c, err := dialFake(t, f)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := c.IsSubscribed(t.Context(), "", 1); err == nil {
		t.Error("IsSubscribed = nil error for an empty repo")
	}
	if _, err := c.SetSubscription(t.Context(), "o/r", 0, true); err == nil {
		t.Error("SetSubscription = nil error for pr 0")
	}
}
