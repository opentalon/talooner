package github

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// Placeholder is what a redacted value is replaced with.
const Placeholder = "[REDACTED]"

// tokenPatterns are the key shapes that must never reach a log, whether or not
// anyone registered them. GitHub's own prefixes plus the `Bearer <x>` header
// form. Deliberately not a generic "long hex string" rule: commit shas are 40
// hex characters and are logged on purpose.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
}

// Redactor rewrites secrets out of a string. It carries the literals it was
// given — the job's GITHUB_TOKEN, the cluster key — because pattern matching
// alone cannot recognise an arbitrary secret (auth.md, "Redaction on the log
// path").
type Redactor struct {
	literals []string
}

// NewRedactor returns a Redactor for the given secrets. Empty and very short
// values are dropped: redacting "a" would turn every log line into noise.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) >= 8 {
			r.literals = append(r.literals, s)
		}
	}
	return r
}

// String returns s with every known secret and key-shaped value replaced.
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

// Error returns err's message with secrets replaced. The result is a plain
// error: unwrapping a redacted error would hand the caller the unredacted one
// back, so the chain stops here.
func (r *Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	return redactedError(r.String(err.Error()))
}

type redactedError string

func (e redactedError) Error() string { return string(e) }

// RedactHandler wraps h so every message and string attribute passes through r
// on the way out. Filtering at the handler is the point: a new call site cannot
// leak a token by forgetting to redact.
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

// redactAttr rewrites a's value. Strings and errors are rewritten in place;
// groups are walked; anything else is resolved to its string form only when it
// is not already a number or a bool, so ints stay ints in a JSON log.
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
