package action

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

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
	ErrUnknownVerb   = errors.New("unknown verb")
	ErrNoExecutor    = errors.New("no executor for verb")
	ErrInvalidAction = errors.New("invalid action")
)

type Action struct {
	Verb     Verb
	Target   string
	Text     string
	Assignee string
	Name     string
}

var verbNames = map[taloonerpb.Verb]Verb{
	taloonerpb.Verb_VERB_APPROVE: VerbApprove,
	taloonerpb.Verb_VERB_BLOCK:   VerbBlock,
	taloonerpb.Verb_VERB_COMMENT: VerbComment,
	taloonerpb.Verb_VERB_ASSIGN:  VerbAssign,
	taloonerpb.Verb_VERB_REQUIRE: VerbRequire,
	taloonerpb.Verb_VERB_NOTIFY:  VerbNotify,
	taloonerpb.Verb_VERB_EMIT:    VerbEmit,
}

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

type Executor interface {
	Execute(ctx context.Context, a Action) error
}

func Derived(reason string) Executor { return derived(reason) }

type derived string

func (derived) Execute(context.Context, Action) error { return nil }

type Registry map[Verb]Executor

func (r Registry) Validate(actions []Action) error {
	for i, a := range actions {
		if err := r.check(a); err != nil {
			return fmt.Errorf("action %d: %w", i, err)
		}
	}
	return nil
}

func (r Registry) Execute(ctx context.Context, actions []Action) error {
	if err := r.Validate(actions); err != nil {
		return err
	}
	for i, a := range actions {
		if err := r[a.Verb].Execute(ctx, a); err != nil {
			return fmt.Errorf("action %d, %s: %w", i, a.Verb, err)
		}
	}
	return nil
}

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

func (r Registry) Complete() error {
	for _, v := range Verbs {
		if e, ok := r[v]; !ok || e == nil {
			return fmt.Errorf("%w: %q", ErrNoExecutor, v)
		}
	}
	return nil
}

type spec struct {
	verb     Verb
	validate func(Action) error
	describe func(Action) string
}

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

func Describe(a Action) string {
	s, ok := specs[a.Verb]
	if !ok {
		return fmt.Sprintf("unknown verb %q", a.Verb)
	}
	return s.describe(a)
}

func required(v Verb, arg, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s needs %s", ErrInvalidAction, v, arg)
	}
	return nil
}

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
