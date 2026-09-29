package gitlab

import (
	"context"
	"errors"
	"testing"

	"github.com/opentalon/talooner/internal/event"
	"github.com/opentalon/talooner/internal/host"
	gitlabhost "github.com/opentalon/talooner/internal/host/gitlab"
)

var _ NoteFetcher = (*gitlabhost.Client)(nil)

type fakeNoteFetcher struct {
	body, author string
	err          error
	gotOwner     string
	gotRepo      string
	gotIID       int
	gotNoteID    int64
}

func (f *fakeNoteFetcher) Note(ctx context.Context, owner, repo string, mrIID int, noteID int64) (string, string, error) {
	f.gotOwner, f.gotRepo, f.gotIID, f.gotNoteID = owner, repo, mrIID, noteID
	return f.body, f.author, f.err
}

func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func TestFromEnvMergeRequestEvent(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE":            "merge_request_event",
		"CI_MERGE_REQUEST_PROJECT_PATH": "group/subgroup/talooner",
		"CI_MERGE_REQUEST_IID":          "42",
		"CI_COMMIT_SHA":                 "deadbeef",
		"GITLAB_USER_LOGIN":             "zhisme",
	})

	ev, err := FromEnv(context.Background(), &fakeNoteFetcher{})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if ev.Trigger != event.TriggerPullRequest || ev.Action != "synchronize" {
		t.Errorf("trigger/action = %s/%s, want %s/synchronize", ev.Trigger, ev.Action, event.TriggerPullRequest)
	}
	if ev.Owner != "group/subgroup" || ev.Repo != "talooner" {
		t.Errorf("repo = %s/%s, want group/subgroup/talooner", ev.Owner, ev.Repo)
	}
	if ev.PR != 42 {
		t.Errorf("PR = %d, want 42", ev.PR)
	}
	if ev.HeadSHA != "deadbeef" {
		t.Errorf("HeadSHA = %q, want deadbeef", ev.HeadSHA)
	}
	if ev.Actor != "zhisme" {
		t.Errorf("Actor = %q, want zhisme", ev.Actor)
	}
}

func TestFromEnvMergeRequestEventMissingIID(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE":            "merge_request_event",
		"CI_MERGE_REQUEST_PROJECT_PATH": "opentalon/talooner",
		"CI_COMMIT_SHA":                 "deadbeef",
	})

	if _, err := FromEnv(context.Background(), &fakeNoteFetcher{}); err == nil {
		t.Fatal("FromEnv: want error for missing CI_MERGE_REQUEST_IID")
	}
}

func TestFromEnvMergeRequestEventBadIID(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE":            "merge_request_event",
		"CI_MERGE_REQUEST_PROJECT_PATH": "opentalon/talooner",
		"CI_MERGE_REQUEST_IID":          "not-a-number",
		"CI_COMMIT_SHA":                 "deadbeef",
	})

	if _, err := FromEnv(context.Background(), &fakeNoteFetcher{}); err == nil {
		t.Fatal("FromEnv: want error for non-numeric CI_MERGE_REQUEST_IID")
	}
}

func TestFromEnvMergeRequestEventMissingSHA(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE":            "merge_request_event",
		"CI_MERGE_REQUEST_PROJECT_PATH": "opentalon/talooner",
		"CI_MERGE_REQUEST_IID":          "42",
	})

	if _, err := FromEnv(context.Background(), &fakeNoteFetcher{}); err == nil {
		t.Fatal("FromEnv: want error for missing CI_COMMIT_SHA")
	}
}

func TestFromEnvMergeRequestEventBadProjectPath(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE":            "merge_request_event",
		"CI_MERGE_REQUEST_PROJECT_PATH": "no-slash",
		"CI_MERGE_REQUEST_IID":          "42",
		"CI_COMMIT_SHA":                 "deadbeef",
	})

	if _, err := FromEnv(context.Background(), &fakeNoteFetcher{}); err == nil {
		t.Fatal("FromEnv: want error for project path with no namespace")
	}
}

func TestFromEnvTrigger(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE": "trigger",
		"CI_PROJECT_PATH":    "opentalon/talooner",
		"TALOONER_MR_IID":    "7",
		"TALOONER_NOTE_ID":   "987",
	})
	nf := &fakeNoteFetcher{body: "/talooner plan", author: "zhisme"}

	ev, err := FromEnv(context.Background(), nf)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if ev.Trigger != event.TriggerIssueComment {
		t.Errorf("Trigger = %s, want %s", ev.Trigger, event.TriggerIssueComment)
	}
	if ev.Owner != "opentalon" || ev.Repo != "talooner" {
		t.Errorf("repo = %s/%s, want opentalon/talooner", ev.Owner, ev.Repo)
	}
	if ev.PR != 7 {
		t.Errorf("PR = %d, want 7", ev.PR)
	}
	if ev.CommentBody != "/talooner plan" || ev.CommentID != 987 {
		t.Errorf("comment = %d %q", ev.CommentID, ev.CommentBody)
	}
	if ev.Actor != "zhisme" {
		t.Errorf("Actor = %q, want zhisme", ev.Actor)
	}
	if ev.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty", ev.HeadSHA)
	}
	if nf.gotOwner != "opentalon" || nf.gotRepo != "talooner" || nf.gotIID != 7 || nf.gotNoteID != 987 {
		t.Errorf("Note called with %s/%s !%d note %d, want opentalon/talooner !7 note 987",
			nf.gotOwner, nf.gotRepo, nf.gotIID, nf.gotNoteID)
	}
}

func TestFromEnvTriggerMissingVars(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "missing project path",
			env: map[string]string{
				"CI_PIPELINE_SOURCE": "trigger",
				"TALOONER_MR_IID":    "7",
				"TALOONER_NOTE_ID":   "987",
			},
		},
		{
			name: "missing mr iid",
			env: map[string]string{
				"CI_PIPELINE_SOURCE": "trigger",
				"CI_PROJECT_PATH":    "opentalon/talooner",
				"TALOONER_NOTE_ID":   "987",
			},
		},
		{
			name: "missing note id",
			env: map[string]string{
				"CI_PIPELINE_SOURCE": "trigger",
				"CI_PROJECT_PATH":    "opentalon/talooner",
				"TALOONER_MR_IID":    "7",
			},
		},
		{
			name: "non-numeric note id",
			env: map[string]string{
				"CI_PIPELINE_SOURCE": "trigger",
				"CI_PROJECT_PATH":    "opentalon/talooner",
				"TALOONER_MR_IID":    "7",
				"TALOONER_NOTE_ID":   "not-a-number",
			},
		},
		{
			name: "zero mr iid",
			env: map[string]string{
				"CI_PIPELINE_SOURCE": "trigger",
				"CI_PROJECT_PATH":    "opentalon/talooner",
				"TALOONER_MR_IID":    "0",
				"TALOONER_NOTE_ID":   "987",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEnv(t, tt.env)
			if _, err := FromEnv(context.Background(), &fakeNoteFetcher{}); err == nil {
				t.Fatal("FromEnv: want error")
			}
		})
	}
}

func TestFromEnvTriggerFetchError(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE": "trigger",
		"CI_PROJECT_PATH":    "opentalon/talooner",
		"TALOONER_MR_IID":    "7",
		"TALOONER_NOTE_ID":   "987",
	})
	wantErr := errors.New("note not found")

	_, err := FromEnv(context.Background(), &fakeNoteFetcher{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("FromEnv err = %v, want wrapping %v", err, wantErr)
	}
}

func TestFromEnvTriggerSystemNoteIsSkippable(t *testing.T) {
	withEnv(t, map[string]string{
		"CI_PIPELINE_SOURCE": "trigger",
		"CI_PROJECT_PATH":    "opentalon/talooner",
		"TALOONER_MR_IID":    "7",
		"TALOONER_NOTE_ID":   "987",
	})

	_, err := FromEnv(context.Background(), &fakeNoteFetcher{err: host.ErrNotFound})
	if !errors.Is(err, event.ErrUnhandled) {
		t.Fatalf("FromEnv err = %v, want wrapping event.ErrUnhandled", err)
	}
	if !event.Skip(err) {
		t.Fatalf("event.Skip(%v) = false, want true", err)
	}
}

func TestFromEnvMissingPipelineSource(t *testing.T) {
	if _, err := FromEnv(context.Background(), &fakeNoteFetcher{}); err == nil {
		t.Fatal("FromEnv: want error for missing CI_PIPELINE_SOURCE")
	}
}

func TestFromEnvUnhandledPipelineSource(t *testing.T) {
	withEnv(t, map[string]string{"CI_PIPELINE_SOURCE": "schedule"})

	_, err := FromEnv(context.Background(), &fakeNoteFetcher{})
	if !errors.Is(err, event.ErrUnhandled) {
		t.Fatalf("FromEnv err = %v, want wrapping event.ErrUnhandled", err)
	}
	if !event.Skip(err) {
		t.Fatalf("event.Skip(%v) = false, want true", err)
	}
}
