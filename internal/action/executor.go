// Package action performs the verbs the plugin returned. It is the "what to do
// to GitHub" half of a run; deciding whether to evaluate at all is command's,
// and the two are deliberately separate packages (architecture.md, "command/
// and action/ are not the same concept").
//
// Arguments arrive already resolved. `do assign "pr" attr "user.owner"` reaches
// here carrying @alice, and "{attr.user.owner} owns this" is already
// interpolated, so no executor ever looks a fact up (actions.md).
//
// Two properties this package exists to hold:
//
//   - An unknown verb is a hard error, never a no-op. The language accepts any
//     verb, so a misspelled `do aprove "pr"` parses cleanly; validate_ruleset
//     rejects it cluster-side and this is the second gate. A verb that reaches
//     no executor looks exactly like a rule that never matched, which is the one
//     failure nobody would notice.
//   - The file set equals the verb set. One file per verb, named after it, with
//     a test asserting the two match — adding a verb plugin-side without adding
//     an executor here is drift that would otherwise surface as silence.
package action

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// Verb is one action of the closed vocabulary, spelled the way a ruleset spells
// it: the plugin returns approve, the file is approve.go, the registry key is
// "approve".
type Verb string

const (
	VerbApprove Verb = "approve"
	VerbBlock   Verb = "block"
	VerbComment Verb = "comment"
	VerbAssign  Verb = "assign"
	VerbRequire Verb = "require"
	VerbNotify  Verb = "notify"
	VerbEmit    Verb = "emit"
)

// Verbs is the whole vocabulary, in the order actions.md lists it.
var Verbs = []Verb{
	VerbApprove,
	VerbBlock,
	VerbComment,
	VerbAssign,
	VerbRequire,
	VerbNotify,
	VerbEmit,
}

var (
	// ErrUnknownVerb is a verb outside the vocabulary, including the unset one.
	ErrUnknownVerb = errors.New("unknown verb")
	// ErrNoExecutor is a known verb with nothing registered to perform it. It is
	// separate from ErrUnknownVerb because it is a different bug: the plugin and
	// the bot agree on the vocabulary and this registry was built short.
	ErrNoExecutor = errors.New("no executor for verb")
	// ErrInvalidAction is an action whose arguments cannot be performed — an
	// assign with no assignee, a comment with no text.
	ErrInvalidAction = errors.New("invalid action")
)

// Action is one action to perform, with every argument already resolved. It is
// the local shape of taloonerpb.Action: the wire type stays at the seam so a
// cluster that adds a field does not ripple into every executor signature.
type Action struct {
	Verb     Verb
	Target   string
	Text     string
	Assignee string
	Name     string
}

// verbNames maps the wire enum to the vocabulary. VERB_UNSPECIFIED is absent on
// purpose: a response carrying it is a contract violation, not a default.
var verbNames = map[taloonerpb.Verb]Verb{
	taloonerpb.Verb_VERB_APPROVE: VerbApprove,
	taloonerpb.Verb_VERB_BLOCK:   VerbBlock,
	taloonerpb.Verb_VERB_COMMENT: VerbComment,
	taloonerpb.Verb_VERB_ASSIGN:  VerbAssign,
	taloonerpb.Verb_VERB_REQUIRE: VerbRequire,
	taloonerpb.Verb_VERB_NOTIFY:  VerbNotify,
	taloonerpb.Verb_VERB_EMIT:    VerbEmit,
}

// FromProto converts one wire action. An unspecified or unrecognised verb is an
// error rather than a skipped entry — a newer cluster returning a verb this
// build has never heard of must stop the run, not drop the action.
func FromProto(a *taloonerpb.Action) (Action, error) {
	if a == nil {
		return Action{}, fmt.Errorf("%w: action is missing", ErrInvalidAction)
	}
	v, ok := verbNames[a.GetVerb()]
	if !ok {
		return Action{}, fmt.Errorf("%w: %s", ErrUnknownVerb, a.GetVerb())
	}
	return Action{
		Verb:     v,
		Target:   a.GetTarget(),
		Text:     a.GetText(),
		Assignee: a.GetAssignee(),
		Name:     a.GetName(),
	}, nil
}

// FromProtos converts a whole response's actions. One bad action fails the set:
// the actions are a single verdict, and performing the half that decoded would
// write an incomplete one.
func FromProtos(as []*taloonerpb.Action) ([]Action, error) {
	out := make([]Action, 0, len(as))
	for i, a := range as {
		got, err := FromProto(a)
		if err != nil {
			return nil, fmt.Errorf("action %d: %w", i, err)
		}
		out = append(out, got)
	}
	return out, nil
}

// Executor performs one action. The GitHub implementation writes; the printer
// implementation renders what would be written, and rules plan (F4) is that
// implementation swapped in here rather than a parallel path that can drift.
type Executor interface {
	Execute(ctx context.Context, a Action) error
}

// Registry maps every verb to the executor that performs it.
type Registry map[Verb]Executor

// Execute performs a whole action set: everything is validated first, then
// performed in order. The two passes matter — an action set is one verdict, and
// a batch that dies halfway through leaves GitHub holding half of it.
func (r Registry) Execute(ctx context.Context, actions []Action) error {
	for i, a := range actions {
		if err := r.check(a); err != nil {
			return fmt.Errorf("action %d: %w", i, err)
		}
	}
	for i, a := range actions {
		if err := r[a.Verb].Execute(ctx, a); err != nil {
			return fmt.Errorf("action %d, %s: %w", i, a.Verb, err)
		}
	}
	return nil
}

// check reports whether one action can be performed at all.
func (r Registry) check(a Action) error {
	s, ok := specs[a.Verb]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownVerb, a.Verb)
	}
	e, ok := r[a.Verb]
	if !ok || e == nil {
		return fmt.Errorf("%w: %q", ErrNoExecutor, a.Verb)
	}
	return s.validate(a)
}

// Complete reports whether the registry covers the whole vocabulary. A registry
// built short only fails on the run where the missing verb happens to fire, so
// it is worth asking at construction.
func (r Registry) Complete() error {
	for _, v := range Verbs {
		if e, ok := r[v]; !ok || e == nil {
			return fmt.Errorf("%w: %q", ErrNoExecutor, v)
		}
	}
	return nil
}

// spec is one verb's half of the package: what its arguments must carry and how
// it reads in a plan. Each lives in the file named after its verb.
type spec struct {
	verb     Verb
	validate func(Action) error
	describe func(Action) string
}

// specs is the vocabulary, assembled from the per-verb files.
var specs = func() map[Verb]spec {
	m := make(map[Verb]spec, len(Verbs))
	for _, s := range []spec{
		approveSpec,
		blockSpec,
		commentSpec,
		assignSpec,
		requireSpec,
		notifySpec,
		emitSpec,
	} {
		m[s.verb] = s
	}
	return m
}()

// Describe renders one action the way a plan prints it. An action outside the
// vocabulary renders as unknown rather than as nothing, because a plan that
// silently omits a line is the same lie as an executor that silently skips one.
func Describe(a Action) string {
	s, ok := specs[a.Verb]
	if !ok {
		return fmt.Sprintf("unknown verb %q", a.Verb)
	}
	return s.describe(a)
}

// required is the check every verb repeats: an argument the action cannot be
// performed without.
func required(v Verb, arg, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s needs %s", ErrInvalidAction, v, arg)
	}
	return nil
}

// summarize renders free text as one plan line. A comment body is markdown and
// can be a page long; the plan says which comment, not what it says.
func summarize(text string) string {
	const limit = 60
	line := text
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return line
}
