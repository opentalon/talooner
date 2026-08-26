package run

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/cluster"
)

// writeMethod is every HTTP verb a GitHub call to change state uses. Plan's
// whole point is that none of these are ever hit.
func writeMethod(method string) bool {
	switch method {
	case "POST", "PATCH", "PUT", "DELETE":
		return true
	default:
		return false
	}
}

func TestPlanRendersActionsAndWritesNothing(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionEvaluatePR: &taloonerpb.EvaluatePrResponse{
			Plan: []*taloonerpb.Action{
				{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"},
				{Verb: taloonerpb.Verb_VERB_COMMENT, Target: "pr", Text: "looks fine"},
			},
		},
	}}
	gh := &fakeGitHub{}

	var out bytes.Buffer
	r := Runner{GitHub: gh.client(t), Cluster: dialFake(t, f)}
	if err := r.Plan(context.Background(), "opentalon", "talooner", 42, &out); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "approve") || !strings.Contains(got, "looks fine") {
		t.Errorf("plan output = %q, want lines for both actions", got)
	}

	if args := f.argsOf(t, cluster.ActionEvaluatePR); args["mode"] != string(cluster.ModePlan) {
		t.Errorf("mode = %q, want plan", args["mode"])
	}

	for _, m := range gh.methods {
		if writeMethod(m) {
			t.Errorf("GitHub saw a %s call; Plan must never write", m)
		}
	}
}

func TestPlanWithNoActionsRendersNothingAndSucceeds(t *testing.T) {
	f := &fakeCluster{answers: map[string]proto.Message{
		cluster.ActionEvaluatePR: &taloonerpb.EvaluatePrResponse{},
	}}
	gh := &fakeGitHub{}

	var out bytes.Buffer
	r := Runner{GitHub: gh.client(t), Cluster: dialFake(t, f)}
	if err := r.Plan(context.Background(), "opentalon", "talooner", 42, &out); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("plan output = %q, want empty for no actions", out.String())
	}
}

// A ruleset the plugin refuses fails Plan the same way it fails a real run —
// no annotated check run, since Plan writes nothing at all, just the error.
func TestPlanWithBrokenRulesetFails(t *testing.T) {
	f := &fakeCluster{
		answers:  map[string]proto.Message{},
		failures: map[string]string{cluster.ActionEvaluatePR: "talooner: evaluate ruleset: compile failed"},
	}
	gh := &fakeGitHub{}

	var out bytes.Buffer
	r := Runner{GitHub: gh.client(t), Cluster: dialFake(t, f)}
	if err := r.Plan(context.Background(), "opentalon", "talooner", 42, &out); err == nil {
		t.Fatal("Plan = nil, want the plugin's refusal")
	}
	for _, m := range gh.methods {
		if writeMethod(m) {
			t.Errorf("GitHub saw a %s call on a failed plan; Plan must never write", m)
		}
	}
}
