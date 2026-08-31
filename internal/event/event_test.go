package event

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIssueCommentOnPullRequest(t *testing.T) {
	body := `{
	  "action": "created",
	  "repository": {"full_name": "opentalon/talooner"},
	  "sender": {"login": "zhisme"},
	  "issue": {"number": 42, "pull_request": {"url": "https://api.github.com/repos/opentalon/talooner/pulls/42"}},
	  "comment": {"id": 987, "body": "!talooner /review"}
	}`

	ev, err := Parse(TriggerIssueComment, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Owner != "opentalon" || ev.Repo != "talooner" {
		t.Errorf("repo = %s/%s, want opentalon/talooner", ev.Owner, ev.Repo)
	}
	if ev.PR != 42 {
		t.Errorf("PR = %d, want 42", ev.PR)
	}
	if ev.Actor != "zhisme" {
		t.Errorf("Actor = %q, want zhisme", ev.Actor)
	}
	if ev.CommentBody != "!talooner /review" || ev.CommentID != 987 {
		t.Errorf("comment = %d %q", ev.CommentID, ev.CommentBody)
	}
	// issue_comment carries no head sha; the caller must fetch it.
	if ev.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty", ev.HeadSHA)
	}
}

func TestParsePullRequestShapes(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		body    string
		wantPR  int
		wantSHA string
	}{
		{
			name:    "synchronize",
			trigger: TriggerPullRequest,
			body: `{"action":"synchronize","number":7,
			        "repository":{"full_name":"opentalon/talooner"},
			        "sender":{"login":"zhisme"},
			        "pull_request":{"number":7,"head":{"sha":"deadbeef"}}}`,
			wantPR:  7,
			wantSHA: "deadbeef",
		},
		{
			// A closed payload with no head object at all: the field is left
			// empty rather than panicking on a nil head.
			name:    "closed without head",
			trigger: TriggerPullRequest,
			body: `{"action":"closed","number":7,
			        "repository":{"full_name":"opentalon/talooner"},
			        "pull_request":{"number":7}}`,
			wantPR:  7,
			wantSHA: "",
		},
		{
			// pull_request payloads carry the number twice; the nested one can
			// be absent in trimmed payloads.
			name:    "number only at top level",
			trigger: TriggerPullRequest,
			body: `{"action":"reopened","number":9,
			        "repository":{"full_name":"opentalon/talooner"},
			        "pull_request":{"head":{"sha":"abc"}}}`,
			wantPR:  9,
			wantSHA: "abc",
		},
		{
			name:    "review submitted",
			trigger: TriggerPullRequestReview,
			body: `{"action":"submitted",
			        "repository":{"full_name":"opentalon/talooner"},
			        "sender":{"login":"reviewer"},
			        "pull_request":{"number":11,"head":{"sha":"cafe"}},
			        "review":{"state":"approved"}}`,
			wantPR:  11,
			wantSHA: "cafe",
		},
		{
			name:    "check suite completed",
			trigger: TriggerCheckSuite,
			body: `{"action":"completed",
			        "repository":{"full_name":"opentalon/talooner"},
			        "check_suite":{"head_sha":"f00d","pull_requests":[{"number":13}]}}`,
			wantPR:  13,
			wantSHA: "f00d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := Parse(tt.trigger, strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.PR != tt.wantPR {
				t.Errorf("PR = %d, want %d", ev.PR, tt.wantPR)
			}
			if ev.HeadSHA != tt.wantSHA {
				t.Errorf("HeadSHA = %q, want %q", ev.HeadSHA, tt.wantSHA)
			}
		})
	}
}

func TestParseSkips(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		body    string
		want    error
	}{
		{
			name:    "comment on a plain issue",
			trigger: TriggerIssueComment,
			body: `{"action":"created",
			        "repository":{"full_name":"opentalon/talooner"},
			        "issue":{"number":5},
			        "comment":{"id":1,"body":"!talooner /review"}}`,
			want: ErrNotPullRequest,
		},
		{
			name:    "edited comment",
			trigger: TriggerIssueComment,
			body: `{"action":"edited",
			        "repository":{"full_name":"opentalon/talooner"},
			        "issue":{"number":5,"pull_request":{"url":"u"}},
			        "comment":{"id":1,"body":"!talooner /review"}}`,
			want: ErrUnhandled,
		},
		{
			// Talooner waits to be asked: opened is not a trigger to act on.
			name:    "pull request opened",
			trigger: TriggerPullRequest,
			body: `{"action":"opened","number":7,
			        "repository":{"full_name":"opentalon/talooner"},
			        "pull_request":{"number":7,"head":{"sha":"abc"}}}`,
			want: ErrUnhandled,
		},
		{
			name:    "check suite with no pull request",
			trigger: TriggerCheckSuite,
			body: `{"action":"completed",
			        "repository":{"full_name":"opentalon/talooner"},
			        "check_suite":{"head_sha":"f00d","pull_requests":[]}}`,
			want: ErrNoPullRequest,
		},
		{
			name:    "unhandled trigger",
			trigger: "push",
			body:    `{"repository":{"full_name":"opentalon/talooner"}}`,
			want:    ErrUnhandled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.trigger, strings.NewReader(tt.body))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !Skip(err) {
				t.Errorf("Skip(%v) = false, want true", err)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		body    string
		wantIn  string
	}{
		{
			name:    "malformed json",
			trigger: TriggerIssueComment,
			body:    `{"action": "created"`,
			wantIn:  "decode payload for trigger issue_comment",
		},
		{
			name:    "empty payload",
			trigger: TriggerIssueComment,
			body:    `{}`,
			wantIn:  "no repository.full_name",
		},
		{
			name:    "full_name is not owner/name",
			trigger: TriggerIssueComment,
			body:    `{"action":"created","repository":{"full_name":"talooner"}}`,
			wantIn:  "is not owner/name",
		},
		{
			name:    "missing comment object",
			trigger: TriggerIssueComment,
			body: `{"action":"created",
			        "repository":{"full_name":"opentalon/talooner"},
			        "issue":{"number":5,"pull_request":{"url":"u"}}}`,
			wantIn: "has no comment",
		},
		{
			name:    "pull_request payload with no pull_request",
			trigger: TriggerPullRequest,
			body:    `{"action":"closed","repository":{"full_name":"opentalon/talooner"}}`,
			wantIn:  "has no pull_request",
		},
		{
			name:    "zero pull request number",
			trigger: TriggerPullRequest,
			body: `{"action":"closed",
			        "repository":{"full_name":"opentalon/talooner"},
			        "pull_request":{"number":0}}`,
			wantIn: "has no pull request number",
		},
		{
			name:    "empty trigger",
			trigger: "",
			body:    `{"repository":{"full_name":"opentalon/talooner"}}`,
			wantIn:  "GITHUB_EVENT_NAME is not set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.trigger, strings.NewReader(tt.body))
			if err == nil {
				t.Fatal("Parse: want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantIn)
			}
			if Skip(err) {
				t.Errorf("Skip(%v) = true, want false", err)
			}
		})
	}
}

func TestFromEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	body := `{"action":"created",
	          "repository":{"full_name":"opentalon/talooner"},
	          "sender":{"login":"zhisme"},
	          "issue":{"number":42,"pull_request":{"url":"u"}},
	          "comment":{"id":1,"body":"!talooner /review"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITHUB_EVENT_NAME", TriggerIssueComment)
	t.Setenv("GITHUB_EVENT_PATH", path)

	ev, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if ev.PR != 42 {
		t.Errorf("PR = %d, want 42", ev.PR)
	}
}

func TestFromEnvUnsetPath(t *testing.T) {
	t.Setenv("GITHUB_EVENT_NAME", TriggerIssueComment)
	t.Setenv("GITHUB_EVENT_PATH", "")

	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_EVENT_PATH is not set") {
		t.Fatalf("err = %v, want GITHUB_EVENT_PATH is not set", err)
	}
}

func TestFromEnvMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv("GITHUB_EVENT_NAME", TriggerIssueComment)
	t.Setenv("GITHUB_EVENT_PATH", path)

	_, err := FromEnv()
	if err == nil {
		t.Fatal("FromEnv: want error, got nil")
	}
	// The error has to name the file — a bare "no such file" tells nobody which.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("err = %q, want it to name %s", err, path)
	}
	if Skip(err) {
		t.Error("Skip = true, want false")
	}
}

// A skip that reaches FromEnv keeps its sentinel through the path wrapping, so
// the entrypoint can still exit 0 on it.
func TestFromEnvPreservesSkip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	body := `{"action":"created",
	          "repository":{"full_name":"opentalon/talooner"},
	          "issue":{"number":5},
	          "comment":{"id":1,"body":"hello"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITHUB_EVENT_NAME", TriggerIssueComment)
	t.Setenv("GITHUB_EVENT_PATH", path)

	_, err := FromEnv()
	if !errors.Is(err, ErrNotPullRequest) || !Skip(err) {
		t.Fatalf("err = %v, want a skip on ErrNotPullRequest", err)
	}
}
