package action

import (
	"context"
	"fmt"
	"io"
)

// Printer renders actions instead of performing them. It is the dry run: `rules
// plan` (F4) is this executor swapped into the same registry the real run uses,
// not a second code path that can drift from what execution actually does, and
// E2 evaluates an untrusted head-branch ruleset through it.
//
// It holds a writer and nothing else. "Prints instead of writing" is then a
// property of the type rather than a discipline — there is no client here to
// write with.
type Printer struct {
	w io.Writer
}

// NewPrinter renders to w.
func NewPrinter(w io.Writer) *Printer {
	return &Printer{w: w}
}

// Execute prints one line describing the action. A failing write is returned
// rather than dropped: a plan that silently lost a line is a plan that lies.
func (p *Printer) Execute(_ context.Context, a Action) error {
	if _, err := fmt.Fprintln(p.w, Describe(a)); err != nil {
		return fmt.Errorf("write plan line for %s: %w", a.Verb, err)
	}
	return nil
}

// Printing is a complete registry that performs nothing. Every verb maps to the
// same printer, so a verb added to the vocabulary is plannable the moment its
// file exists, with no second place to register it.
func Printing(w io.Writer) Registry {
	p := NewPrinter(w)
	r := make(Registry, len(Verbs))
	for _, v := range Verbs {
		r[v] = p
	}
	return r
}
