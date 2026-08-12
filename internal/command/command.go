// Package command finds a Talooner command in a PR comment and says who is
// allowed to run one.
//
// The write-access gate is the reason this is its own package. Without it any
// GitHub account could comment `@talooner /review` on a public repo and spend
// the maintainer's LLM budget (actions.md, "Workflow permissions").
//
// Callers run the two halves in this order:
//
//	cmd, err := command.Parse(handle, ev.CommentBody)
//	// ErrNoCommand: exit 0, no API calls, no reply.
//	// Anything else, including a parse error: check access first.
//	if err := command.Authorize(ctx, gh, ev.Owner, ev.Repo, ev.Actor); err != nil { ... }
//	// ErrNotAuthorized: exit 0, and no reply — a reply would leak that the bot
//	// is installed and hand an unauthorised account a way to make it post.
//
// Only after Authorize returns nil may a parse error be answered with a
// comment. Silent reports which failures get no reply at all.
package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DefaultHandle is the string Talooner answers to. It is not a GitHub mention,
// just a string matched in the body; tenants can change it in config.yaml
// (auth.md, "Identity").
const DefaultHandle = "@talooner"

// The verbs Talooner serves (architecture.md, "Commands").
const (
	VerbReview = "review"
	VerbStop   = "stop"
	VerbWhy    = "why"
	VerbPlan   = "plan"
)

var (
	// ErrNoCommand is a comment that addresses Talooner with nothing to do, or
	// does not address it at all. Exit 0, say nothing.
	ErrNoCommand = errors.New("comment carries no command")
	// ErrUnknownCommand is the handle followed by something that is not a verb.
	// Worth exactly one reply listing the valid commands.
	ErrUnknownCommand = errors.New("unknown command")
	// ErrForceUnsupported is `/review --force` before llm_review exists.
	ErrForceUnsupported = errors.New("--force is not available until llm_review lands")
	// ErrNotAuthorized is a command from an account without write access.
	ErrNotAuthorized = errors.New("commander has no write access")
)

// Command is one parsed invocation.
type Command struct {
	Verb string
	// Force is `/review --force`. In v1 Parse rejects it, so this is always
	// false; the field is here because the flag is parsed, not ignored.
	Force bool
	// Line is the comment line the command was read from, for log context.
	Line string
}

// Silent reports whether err should produce no reply on the PR. Everything else
// — an unknown command, a rejected flag, a broken API — is worth saying out
// loud, once the commander has been shown to have write access.
func Silent(err error) bool {
	return errors.Is(err, ErrNoCommand) || errors.Is(err, ErrNotAuthorized)
}

// Usage is the body of the reply to an unknown command.
func Usage(handle string) string {
	return fmt.Sprintf(`Unknown command. Talooner understands:

- `+"`%[1]s /review`"+` — evaluate this PR now and subscribe it
- `+"`%[1]s /stop`"+` — unsubscribe this PR
- `+"`%[1]s /why`"+` — explain the current verdict
- `+"`%[1]s /plan`"+` — evaluate the head-branch ruleset with no writes`, handle)
}

// Parse finds the first command in a comment body.
//
// A command is a line that *starts* with the handle: leading whitespace is
// allowed but nothing else. That is what keeps `as @talooner /review would say`
// and a quoted copy of someone else's comment from re-invoking the bot. Lines
// inside a fenced code block, inside a blockquote, or indented four columns
// (markdown's own code-block rule) are skipped for the same reason.
func Parse(handle, body string) (*Command, error) {
	if handle == "" {
		return nil, errors.New("handle is empty, every line would match")
	}

	fence := "" // the marker that opened the block we are inside, if any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r \t")

		if marker := fenceMarker(line); marker != "" && (fence == "" || fence == marker) {
			if fence == "" {
				fence = marker
			} else {
				fence = ""
			}
			continue
		}
		if fence != "" || indentColumns(line) >= 4 {
			continue
		}

		rest := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(rest, ">") {
			continue
		}
		fields := strings.Fields(rest)
		// A bare handle is a mention, not a command: no reply.
		if len(fields) < 2 || !strings.EqualFold(fields[0], handle) {
			continue
		}
		return parseFields(fields[1:], line)
	}
	return nil, ErrNoCommand
}

func parseFields(args []string, line string) (*Command, error) {
	verb, ok := strings.CutPrefix(args[0], "/")
	if !ok {
		return nil, ErrNoCommand
	}
	switch verb = strings.ToLower(verb); verb {
	case VerbReview, VerbStop, VerbWhy, VerbPlan:
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnknownCommand, args[0])
	}

	args = args[1:]
	// --force is the only flag any verb takes, and only /review takes it.
	if verb == VerbReview && len(args) == 1 && args[0] == "--force" {
		return nil, ErrForceUnsupported
	}
	if len(args) > 0 {
		return nil, fmt.Errorf("%w: /%s does not take %s", ErrUnknownCommand, verb, args[0])
	}
	return &Command{Verb: verb, Line: line}, nil
}

// fenceMarker returns the fence a line opens or closes, or "".
func fenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker
		}
	}
	return ""
}

// indentColumns counts a line's leading whitespace in columns, a tab being four.
func indentColumns(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// PermissionChecker reports a user's access to a repo. internal/github (B3)
// implements it against the permission API; the gate is checked on every
// command rather than cached, so revoking access takes effect at once.
type PermissionChecker interface {
	HasWriteAccess(ctx context.Context, owner, repo, login string) (bool, error)
}

// Authorize reports whether login may run commands on owner/repo.
//
// A checker failure comes back as a wrapped error and never as ErrNotAuthorized:
// a permission API that 500s must fail the run loudly rather than silently drop
// a maintainer's command.
func Authorize(ctx context.Context, c PermissionChecker, owner, repo, login string) error {
	if login == "" {
		return errors.New("cannot authorize a command with no actor")
	}
	ok, err := c.HasWriteAccess(ctx, owner, repo, login)
	if err != nil {
		return fmt.Errorf("check write access for %s on %s/%s: %w", login, owner, repo, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s on %s/%s", ErrNotAuthorized, login, owner, repo)
	}
	return nil
}
