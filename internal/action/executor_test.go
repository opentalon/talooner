package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// recorder is an Executor that performs nothing and remembers what it was asked
// to do, so a test can assert on the actions that reached it — and, more often,
// on the ones that did not.
type recorder struct {
	got  []Action
	fail error
}

func (r *recorder) Execute(_ context.Context, a Action) error {
	r.got = append(r.got, a)
	return r.fail
}

// registryOf builds a complete registry over one executor.
func registryOf(e Executor) Registry {
	r := make(Registry, len(Verbs))
	for _, v := range Verbs {
		r[v] = e
	}
	return r
}

func TestFromProtoCarriesEveryVerbAndArgument(t *testing.T) {
	for wire, want := range verbNames {
		in := &taloonerpb.Action{
			Verb:     wire,
			Target:   "pr",
			Text:     "text",
			Assignee: "@alice",
			Name:     "needs_design",
		}
		got, err := FromProto(in)
		if err != nil {
			t.Fatalf("FromProto(%s) returned %v, want no error", wire, err)
		}
		if got.Verb != want {
			t.Errorf("FromProto(%s).Verb = %q, want %q", wire, got.Verb, want)
		}
		if got.Target != "pr" || got.Text != "text" || got.Assignee != "@alice" || got.Name != "needs_design" {
			t.Errorf("FromProto(%s) = %+v, want every argument copied", wire, got)
		}
	}
}

// The unset verb is what a response with a missing field decodes to, and it is
// exactly the case that must not pass through as something performable.
func TestFromProtoRejectsUnspecifiedVerb(t *testing.T) {
	_, err := FromProto(&taloonerpb.Action{Verb: taloonerpb.Verb_VERB_UNSPECIFIED, Target: "pr"})
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("FromProto(VERB_UNSPECIFIED) returned %v, want ErrUnknownVerb", err)
	}
}

// A verb this build has never heard of means the cluster is newer than the bot.
// Dropping it would execute a partial verdict, so it is a hard error.
func TestFromProtoRejectsVerbFromTheFuture(t *testing.T) {
	_, err := FromProto(&taloonerpb.Action{Verb: taloonerpb.Verb(99), Target: "pr"})
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("FromProto(99) returned %v, want ErrUnknownVerb", err)
	}
}

func TestFromProtoRejectsNilAction(t *testing.T) {
	_, err := FromProto(nil)
	if !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("FromProto(nil) returned %v, want ErrInvalidAction", err)
	}
}

// The action list is one verdict. Converting the good half and dropping the rest
// would hand the executors an incomplete decision.
func TestFromProtosFailsTheWholeSet(t *testing.T) {
	got, err := FromProtos([]*taloonerpb.Action{
		{Verb: taloonerpb.Verb_VERB_APPROVE, Target: "pr"},
		{Verb: taloonerpb.Verb_VERB_UNSPECIFIED},
	})
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("FromProtos returned %v, want ErrUnknownVerb", err)
	}
	if got != nil {
		t.Errorf("FromProtos returned %v, want nil actions alongside the error", got)
	}
	if !strings.Contains(err.Error(), "action 1") {
		t.Errorf("FromProtos error = %q, want it to name the offending index", err)
	}
}

func TestExecuteRunsEveryActionInOrder(t *testing.T) {
	rec := &recorder{}
	actions := []Action{
		{Verb: VerbBlock, Target: "pr.merge"},
		{Verb: VerbComment, Target: "pr", Text: "no description"},
		{Verb: VerbEmit, Name: "needs_design"},
	}
	if err := registryOf(rec).Execute(context.Background(), actions); err != nil {
		t.Fatalf("Execute returned %v, want no error", err)
	}
	if len(rec.got) != len(actions) {
		t.Fatalf("executor saw %d actions, want %d", len(rec.got), len(actions))
	}
	for i := range actions {
		if rec.got[i].Verb != actions[i].Verb {
			t.Errorf("action %d was %q, want %q", i, rec.got[i].Verb, actions[i].Verb)
		}
	}
}

// A verb that reaches no executor looks exactly like a rule that never matched,
// which is why it stops the run instead of being skipped.
func TestExecuteRejectsUnknownVerb(t *testing.T) {
	rec := &recorder{}
	err := registryOf(rec).Execute(context.Background(), []Action{{Verb: "aprove", Target: "pr"}})
	if !errors.Is(err, ErrUnknownVerb) {
		t.Fatalf("Execute returned %v, want ErrUnknownVerb", err)
	}
	if len(rec.got) != 0 {
		t.Errorf("executor saw %v, want nothing performed", rec.got)
	}
}

// A registry built short is a different bug from an unknown verb: both sides
// agree on the vocabulary and this one was assembled wrong.
func TestExecuteRejectsAVerbWithNoExecutor(t *testing.T) {
	rec := &recorder{}
	r := registryOf(rec)
	delete(r, VerbNotify)

	err := r.Execute(context.Background(), []Action{{Verb: VerbNotify, Target: "#eng", Text: "hi"}})
	if !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("Execute returned %v, want ErrNoExecutor", err)
	}
	if !strings.Contains(err.Error(), string(VerbNotify)) {
		t.Errorf("Execute error = %q, want it to name the verb", err)
	}
}

// A nil value in the map is the same hole as a missing key, and would panic
// rather than fail.
func TestExecuteRejectsANilExecutor(t *testing.T) {
	r := registryOf(&recorder{})
	r[VerbApprove] = nil

	err := r.Execute(context.Background(), []Action{{Verb: VerbApprove, Target: "pr"}})
	if !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("Execute returned %v, want ErrNoExecutor", err)
	}
}

// The two passes are the point: an action set is one verdict, so a bad action
// anywhere in it must stop the whole thing before GitHub holds half of it.
func TestExecuteValidatesEverythingBeforePerformingAnything(t *testing.T) {
	rec := &recorder{}
	err := registryOf(rec).Execute(context.Background(), []Action{
		{Verb: VerbApprove, Target: "pr"},
		{Verb: VerbAssign, Target: "pr"}, // resolved to nobody
	})
	if !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("Execute returned %v, want ErrInvalidAction", err)
	}
	if len(rec.got) != 0 {
		t.Errorf("executor performed %v, want nothing performed at all", rec.got)
	}
	if !strings.Contains(err.Error(), "action 1") {
		t.Errorf("Execute error = %q, want it to name the offending index", err)
	}
}

func TestExecuteReportsWhichActionFailed(t *testing.T) {
	rec := &recorder{fail: errors.New("422 unprocessable")}
	err := registryOf(rec).Execute(context.Background(), []Action{{Verb: VerbBlock, Target: "pr.merge"}})
	if err == nil {
		t.Fatal("Execute returned no error, want the executor's failure")
	}
	if !strings.Contains(err.Error(), "block") || !strings.Contains(err.Error(), "422 unprocessable") {
		t.Errorf("Execute error = %q, want the verb and the underlying failure", err)
	}
}

func TestCompleteRejectsAPartialRegistry(t *testing.T) {
	r := registryOf(&recorder{})
	if err := r.Complete(); err != nil {
		t.Fatalf("Complete on a full registry returned %v, want no error", err)
	}
	delete(r, VerbRequire)
	if err := r.Complete(); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("Complete returned %v, want ErrNoExecutor", err)
	}
}

// Adding a verb plugin-side without adding an executor here is drift that would
// otherwise surface as an action nobody performs. The file set is the check
// because the file name is the verb.
func TestEveryVerbHasItsOwnFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	// The files that are not verbs: the interface, the dry run, and the E2
	// fork-plan comparison.
	infrastructure := map[string]bool{"executor.go": true, "printer.go": true, "diff.go": true}

	files := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") || infrastructure[name] {
			continue
		}
		files[name] = true
	}

	for _, v := range Verbs {
		name := string(v) + ".go"
		if !files[name] {
			t.Errorf("verb %q has no %s", v, name)
		}
		delete(files, name)
	}
	for name := range files {
		t.Errorf("%s is not a verb — either it belongs in executor.go, or Verbs is missing an entry", name)
	}
}

// specs is what the registry validates and describes through, so a verb absent
// from it is a verb with no arguments checked at all.
func TestEveryVerbHasASpec(t *testing.T) {
	if len(specs) != len(Verbs) {
		t.Errorf("specs holds %d verbs, Verbs holds %d", len(specs), len(Verbs))
	}
	for _, v := range Verbs {
		s, ok := specs[v]
		if !ok {
			t.Errorf("verb %q has no spec", v)
			continue
		}
		if s.verb != v {
			t.Errorf("spec under key %q declares verb %q", v, s.verb)
		}
	}
}

func TestValidateRejectsUnperformableArguments(t *testing.T) {
	tests := []struct {
		name string
		a    Action
		want string // an argument the message must name
	}{
		{"approve without a target", Action{Verb: VerbApprove}, "target"},
		{"block without a target", Action{Verb: VerbBlock}, "target"},
		{"comment without a target", Action{Verb: VerbComment, Text: "hi"}, "target"},
		{"comment without text", Action{Verb: VerbComment, Target: "pr"}, "text"},
		{"assign without a target", Action{Verb: VerbAssign, Assignee: "@alice"}, "target"},
		{"assign resolved to nobody", Action{Verb: VerbAssign, Target: "pr"}, "assignee"},
		{"require without a target", Action{Verb: VerbRequire}, "target"},
		{"notify without a target", Action{Verb: VerbNotify, Text: "hi"}, "target"},
		{"notify without text", Action{Verb: VerbNotify, Target: "#eng"}, "text"},
		{"emit without a name", Action{Verb: VerbEmit}, "name"},
	}
	r := registryOf(&recorder{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.check(tt.a)
			if !errors.Is(err, ErrInvalidAction) {
				t.Fatalf("check(%+v) returned %v, want ErrInvalidAction", tt.a, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestValidateAcceptsAFullyResolvedAction(t *testing.T) {
	r := registryOf(&recorder{})
	for _, a := range []Action{
		{Verb: VerbApprove, Target: "pr"},
		{Verb: VerbBlock, Target: "pr.merge"},
		{Verb: VerbComment, Target: "pr", Text: "no description"},
		{Verb: VerbAssign, Target: "pr", Assignee: "@alice"},
		{Verb: VerbRequire, Target: "review.security"},
		{Verb: VerbNotify, Target: "#eng", Text: "deploy blocked"},
		{Verb: VerbEmit, Name: "needs_design"},
	} {
		if err := r.check(a); err != nil {
			t.Errorf("check(%+v) returned %v, want no error", a, err)
		}
	}
}

func TestDescribeNamesAnUnknownVerbRatherThanRenderingNothing(t *testing.T) {
	got := Describe(Action{Verb: "aprove", Target: "pr"})
	if !strings.Contains(got, "aprove") {
		t.Errorf("Describe = %q, want it to name the unknown verb", got)
	}
}

func TestSummarizeKeepsPlanLinesToOneLine(t *testing.T) {
	long := strings.Repeat("é", 100)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a single line is left alone", "no description", "no description"},
		{"only the first line survives", "first line\n\nand a body", "first line"},
		{"carriage returns count too", "first line\r\nbody", "first line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarize(tt.in); got != tt.want {
				t.Errorf("summarize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	// Truncation counts runes, not bytes: cutting mid-rune would print a
	// replacement character in the middle of somebody's comment.
	got := summarize(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("summarize(long) = %q, want it truncated", got)
	}
	if r := []rune(strings.TrimSuffix(got, "…")); len(r) != 60 {
		t.Errorf("summarize(long) kept %d runes, want 60", len(r))
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("summarize(long) = %q, want no rune cut in half", got)
	}
}

func ExampleDescribe() {
	fmt.Println(Describe(Action{Verb: VerbAssign, Target: "pr", Assignee: "@alice"}))
	// Output: assign pr to @alice
}
