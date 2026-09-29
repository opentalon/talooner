package gitlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/opentalon/talooner/internal/event"
)

const (
	sourceMergeRequestEvent = "merge_request_event"
	sourceTrigger           = "trigger"
)

type NoteFetcher interface {
	Note(ctx context.Context, owner, repo string, mrIID int, noteID int64) (body, author string, err error)
}

func FromEnv(ctx context.Context, nf NoteFetcher) (*event.Event, error) {
	source := os.Getenv("CI_PIPELINE_SOURCE")
	if source == "" {
		return nil, errors.New("CI_PIPELINE_SOURCE is not set")
	}

	switch source {
	case sourceMergeRequestEvent:
		return parseMergeRequestEvent()
	case sourceTrigger:
		return parseTrigger(ctx, nf)
	default:
		return nil, fmt.Errorf("%w: pipeline source %s", event.ErrUnhandled, source)
	}
}

func parseMergeRequestEvent() (*event.Event, error) {
	owner, repo, err := splitProjectPath("CI_MERGE_REQUEST_PROJECT_PATH")
	if err != nil {
		return nil, err
	}
	iid, err := requiredInt("CI_MERGE_REQUEST_IID")
	if err != nil {
		return nil, err
	}
	headSHA, err := requiredEnv("CI_COMMIT_SHA")
	if err != nil {
		return nil, err
	}

	return &event.Event{
		Trigger: event.TriggerPullRequest,
		Action:  "synchronize",
		Owner:   owner,
		Repo:    repo,
		PR:      iid,
		HeadSHA: headSHA,
		Actor:   os.Getenv("GITLAB_USER_LOGIN"),
	}, nil
}

func parseTrigger(ctx context.Context, nf NoteFetcher) (*event.Event, error) {
	owner, repo, err := splitProjectPath("CI_PROJECT_PATH")
	if err != nil {
		return nil, err
	}
	iid, err := requiredInt("TALOONER_MR_IID")
	if err != nil {
		return nil, err
	}
	noteID, err := requiredInt64("TALOONER_NOTE_ID")
	if err != nil {
		return nil, err
	}

	body, author, err := nf.Note(ctx, owner, repo, iid, noteID)
	if err != nil {
		return nil, fmt.Errorf("fetch note %d on %s/%s!%d: %w", noteID, owner, repo, iid, err)
	}

	return &event.Event{
		Trigger:     event.TriggerIssueComment,
		Action:      "created",
		Owner:       owner,
		Repo:        repo,
		PR:          iid,
		Actor:       author,
		CommentBody: body,
		CommentID:   noteID,
	}, nil
}

func splitProjectPath(envVar string) (owner, repo string, err error) {
	path, err := requiredEnv(envVar)
	if err != nil {
		return "", "", err
	}
	i := strings.LastIndex(path, "/")
	if i <= 0 || i == len(path)-1 {
		return "", "", fmt.Errorf("%s %q is not namespace/project", envVar, path)
	}
	return path[:i], path[i+1:], nil
}

func requiredEnv(envVar string) (string, error) {
	v := os.Getenv(envVar)
	if v == "" {
		return "", fmt.Errorf("%s is not set", envVar)
	}
	return v, nil
}

func requiredInt(envVar string) (int, error) {
	v, err := requiredEnv(envVar)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s %q is not a positive integer", envVar, v)
	}
	return n, nil
}

func requiredInt64(envVar string) (int64, error) {
	v, err := requiredEnv(envVar)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s %q is not a positive integer", envVar, v)
	}
	return n, nil
}
