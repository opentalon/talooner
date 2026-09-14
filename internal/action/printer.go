package action

import (
	"context"
	"fmt"
	"io"
)

type Printer struct {
	w io.Writer
}

func NewPrinter(w io.Writer) *Printer {
	return &Printer{w: w}
}

func (p *Printer) Execute(_ context.Context, a Action) error {
	if _, err := fmt.Fprintln(p.w, Describe(a)); err != nil {
		return fmt.Errorf("write plan line for %s: %w", a.Verb, err)
	}
	return nil
}

func Printing(w io.Writer) Registry {
	p := NewPrinter(w)
	r := make(Registry, len(Verbs))
	for _, v := range Verbs {
		r[v] = p
	}
	return r
}
