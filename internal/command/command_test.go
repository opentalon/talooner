package command

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseVerbs(t *testing.T) {
	for _, tt := range []struct {
		body string
		want string
	}{
		{"!talooner /review", VerbReview},
		{"!talooner /stop", VerbStop},
		{"!talooner /why", VerbWhy},
		{"!talooner /plan", VerbPlan},
		{"  !talooner /review", VerbReview},
		{"!TALOONER /Review", VerbReview},
		{"!talooner\t/review\t", VerbReview},
		{"looks good\n\n!talooner /review\n", VerbReview},
		{"!talooner /review\r\n", VerbReview},
	} {
		cmd, err := Parse(DefaultHandle, tt.body)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.body, err)
		}
		if cmd.Verb != tt.want {
			t.Errorf("Parse(%q).Verb = %s, want %s", tt.body, cmd.Verb, tt.want)
		}
		if cmd.Force {
			t.Errorf("Parse(%q).Force = true, want false", tt.body)
		}
	}
}

func TestParseNoCommand(t *testing.T) {
	for _, body := range []string{
		"",
		"   \n\n",
		"looks good to me",
		// A bare handle is not a command. Replying with usage to every mention
		// makes the bot noisy in exactly the threads people discuss it in.
		"!talooner",
		"!talooner please have a look",
		// No separator: this is not the handle followed by a command.
		"!talooner/review",
		// A different handle entirely.
		"!talonner /review",
		"!talooner2 /review",
		// The handle must lead the line, so quoting a comment that contains one
		// does not re-invoke it.
		"as !talooner /review would say",
		"see the docs for !talooner /review usage",
	} {
		if _, err := Parse(DefaultHandle, body); !errors.Is(err, ErrNoCommand) {
			t.Errorf("Parse(%q) error = %v, want ErrNoCommand", body, err)
		}
	}
}

func TestParseIgnoresQuotesAndCodeBlocks(t *testing.T) {
	for name, body := range map[string]string{
		"blockquote":       "> !talooner /review\n\nnot me, them",
		"nested quote":     ">> !talooner /stop",
		"quote no space":   ">!talooner /review",
		"backtick fence":   "run this:\n\n```\n!talooner /review\n```\n",
		"tilde fence":      "run this:\n\n~~~\n!talooner /review\n~~~\n",
		"annotated fence":  "```text\n!talooner /review\n```",
		"indented code":    "    !talooner /review",
		"tab indented":     "\t!talooner /review",
		"unclosed fence":   "```\n!talooner /review",
		"inline backticks": "type `!talooner /review` to start",
	} {
		if _, err := Parse(DefaultHandle, body); !errors.Is(err, ErrNoCommand) {
			t.Errorf("%s: Parse(%q) error = %v, want ErrNoCommand", name, body, err)
		}
	}
}

func TestParseAfterFenceCloses(t *testing.T) {
	body := "```\n!talooner /stop\n```\n\n!talooner /review\n"
	cmd, err := Parse(DefaultHandle, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Verb != VerbReview {
		t.Errorf("Verb = %s, want %s — the fenced line must not win", cmd.Verb, VerbReview)
	}
}

func TestParseFirstCommandWins(t *testing.T) {
	cmd, err := Parse(DefaultHandle, "!talooner /why\n!talooner /stop\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Verb != VerbWhy {
		t.Errorf("Verb = %s, want %s", cmd.Verb, VerbWhy)
	}
}

func TestParseForceRejected(t *testing.T) {
	for _, body := range []string{
		"!talooner /review --force",
		"!talooner /review   --force",
	} {
		_, err := Parse(DefaultHandle, body)
		if !errors.Is(err, ErrForceUnsupported) {
			t.Fatalf("Parse(%q) error = %v, want ErrForceUnsupported", body, err)
		}
		if !strings.Contains(err.Error(), "llm_review") {
			t.Errorf("Parse(%q) error = %q, want it to name llm_review", body, err)
		}
	}
}

func TestParseForceOnlyOnReview(t *testing.T) {
	for _, body := range []string{
		"!talooner /plan --force",
		"!talooner /stop --force",
		"!talooner /why --force",
	} {
		if _, err := Parse(DefaultHandle, body); !errors.Is(err, ErrUnknownCommand) {
			t.Errorf("Parse(%q) error = %v, want ErrUnknownCommand", body, err)
		}
	}
}

func TestParseUnknown(t *testing.T) {
	for _, body := range []string{
		"!talooner /frobnicate",
		"!talooner /REVIEWS",
		"!talooner /",
		"!talooner /review --dry-run",
		"!talooner /review extra",
		"!talooner /stop now",
	} {
		_, err := Parse(DefaultHandle, body)
		if !errors.Is(err, ErrUnknownCommand) {
			t.Errorf("Parse(%q) error = %v, want ErrUnknownCommand", body, err)
		}
	}
}

func TestParseCustomHandle(t *testing.T) {
	cmd, err := Parse("@acme-bot", "@acme-bot /review")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Verb != VerbReview {
		t.Errorf("Verb = %s, want %s", cmd.Verb, VerbReview)
	}
	if _, err := Parse("@acme-bot", "!talooner /review"); !errors.Is(err, ErrNoCommand) {
		t.Errorf("default handle honoured under a custom one: %v", err)
	}
}

func TestParseEmptyHandle(t *testing.T) {
	// An empty handle would match every line. Refuse rather than turn every
	// comment on the PR into a command.
	if _, err := Parse("", "!talooner /review"); err == nil || errors.Is(err, ErrNoCommand) {
		t.Errorf("Parse with an empty handle error = %v, want a real error", err)
	}
}

func TestUsageListsEveryVerb(t *testing.T) {
	got := Usage("@acme-bot")
	for _, verb := range []string{VerbReview, VerbStop, VerbWhy, VerbPlan} {
		if !strings.Contains(got, "/"+verb) {
			t.Errorf("Usage() = %q, missing /%s", got, verb)
		}
	}
	if !strings.Contains(got, "@acme-bot") {
		t.Errorf("Usage() = %q, missing the configured handle", got)
	}
}

type checker struct {
	write bool
	err   error
	calls int
	got   [3]string
}

func (c *checker) HasWriteAccess(_ context.Context, owner, repo, login string) (bool, error) {
	c.calls++
	c.got = [3]string{owner, repo, login}
	return c.write, c.err
}

func TestAuthorizeWriteAccess(t *testing.T) {
	c := &checker{write: true}
	if err := Authorize(context.Background(), c, "opentalon", "talooner", "zhisme"); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if c.calls != 1 {
		t.Errorf("checker called %d times, want 1 — the gate is checked per command", c.calls)
	}
	if c.got != [3]string{"opentalon", "talooner", "zhisme"} {
		t.Errorf("checker got %v, want opentalon/talooner zhisme", c.got)
	}
}

func TestAuthorizeDenied(t *testing.T) {
	c := &checker{write: false}
	err := Authorize(context.Background(), c, "opentalon", "talooner", "drive-by")
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("Authorize error = %v, want ErrNotAuthorized", err)
	}
	if !strings.Contains(err.Error(), "drive-by") {
		t.Errorf("Authorize error = %q, want it to name the actor", err)
	}
}

func TestAuthorizeAPIFailureIsNotADenial(t *testing.T) {
	// A permission API that 500s must not read as "no write access": that would
	// silently drop a maintainer's command with exit 0 and no comment.
	boom := errors.New("boom")
	c := &checker{write: true, err: boom}
	err := Authorize(context.Background(), c, "opentalon", "talooner", "zhisme")
	if !errors.Is(err, boom) {
		t.Fatalf("Authorize error = %v, want it to wrap the API error", err)
	}
	if errors.Is(err, ErrNotAuthorized) {
		t.Error("an API failure reported as ErrNotAuthorized — it would be swallowed as a no-op")
	}
}

func TestAuthorizeEmptyActor(t *testing.T) {
	c := &checker{write: true}
	err := Authorize(context.Background(), c, "opentalon", "talooner", "")
	if err == nil {
		t.Fatal("Authorize with no actor returned nil, want an error")
	}
	if c.calls != 0 {
		t.Errorf("checker called %d times for an empty actor, want 0", c.calls)
	}
}

func TestSilentReportsWhichFailuresGetNoReply(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{ErrNotAuthorized, true},
		{ErrNoCommand, true},
		{ErrUnknownCommand, false},
		{ErrForceUnsupported, false},
		{errors.New("boom"), false},
		{nil, false},
	} {
		if got := Silent(tt.err); got != tt.want {
			t.Errorf("Silent(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
