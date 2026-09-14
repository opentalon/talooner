package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const DefaultHandle = "!talooner"

const (
	VerbReview = "review"
	VerbStop   = "stop"
	VerbWhy    = "why"
	VerbPlan   = "plan"
)

var (
	ErrNoCommand        = errors.New("comment carries no command")
	ErrUnknownCommand   = errors.New("unknown command")
	ErrForceUnsupported = errors.New("--force is not available until llm_review lands")
	ErrNotAuthorized    = errors.New("commander has no write access")
)

type Command struct {
	Verb  string
	Force bool
	Line  string
}

func Silent(err error) bool {
	return errors.Is(err, ErrNoCommand) || errors.Is(err, ErrNotAuthorized)
}

func Usage(handle string) string {
	return fmt.Sprintf(`Unknown command. Talooner understands:

- `+"`%[1]s /review`"+` — evaluate this PR now and subscribe it
- `+"`%[1]s /stop`"+` — unsubscribe this PR
- `+"`%[1]s /why`"+` — explain the current verdict
- `+"`%[1]s /plan`"+` — evaluate the head-branch ruleset with no writes`, handle)
}

func Parse(handle, body string) (*Command, error) {
	if handle == "" {
		return nil, errors.New("handle is empty, every line would match")
	}

	fence := ""
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
	if verb == VerbReview && len(args) == 1 && args[0] == "--force" {
		return nil, ErrForceUnsupported
	}
	if len(args) > 0 {
		return nil, fmt.Errorf("%w: /%s does not take %s", ErrUnknownCommand, verb, args[0])
	}
	return &Command{Verb: verb, Line: line}, nil
}

func fenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker
		}
	}
	return ""
}

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

type PermissionChecker interface {
	HasWriteAccess(ctx context.Context, owner, repo, login string) (bool, error)
}

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
