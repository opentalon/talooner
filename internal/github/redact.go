package github

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

const Placeholder = "[REDACTED]"

var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
}

type Redactor struct {
	literals []string
}

func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) >= 8 {
			r.literals = append(r.literals, s)
		}
	}
	return r
}

func (r *Redactor) String(s string) string {
	if r != nil {
		for _, lit := range r.literals {
			s = strings.ReplaceAll(s, lit, Placeholder)
		}
	}
	for _, re := range tokenPatterns {
		s = re.ReplaceAllString(s, Placeholder)
	}
	return s
}

func (r *Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	return redactedError(r.String(err.Error()))
}

type redactedError string

func (e redactedError) Error() string { return string(e) }

func RedactHandler(h slog.Handler, r *Redactor) slog.Handler {
	return &redactHandler{inner: h, redactor: r}
}

type redactHandler struct {
	inner    slog.Handler
	redactor *Redactor
}

func (h *redactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	clean := slog.NewRecord(rec.Time, rec.Level, h.redactor.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = h.redactAttr(a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(clean), redactor: h.redactor}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name), redactor: h.redactor}
}

func (h *redactHandler) redactAttr(a slog.Attr) slog.Attr {
	a.Key = h.redactor.String(a.Key)
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(h.redactor.String(v.String()))
	case slog.KindGroup:
		attrs := v.Group()
		clean := make([]slog.Attr, len(attrs))
		for i, inner := range attrs {
			clean[i] = h.redactAttr(inner)
		}
		a.Value = slog.GroupValue(clean...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			a.Value = slog.StringValue(h.redactor.String(err.Error()))
			return a
		}
		a.Value = slog.StringValue(h.redactor.String(v.String()))
	default:
		a.Value = v
	}
	return a
}
