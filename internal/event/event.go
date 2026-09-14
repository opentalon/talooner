package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	TriggerIssueComment      = "issue_comment"
	TriggerPullRequest       = "pull_request"
	TriggerPullRequestReview = "pull_request_review"
	TriggerCheckSuite        = "check_suite"
)

var (
	ErrNotPullRequest = errors.New("comment is not on a pull request")
	ErrNoPullRequest  = errors.New("event carries no pull request")
	ErrUnhandled      = errors.New("unhandled event")
)

func Skip(err error) bool {
	return errors.Is(err, ErrNotPullRequest) ||
		errors.Is(err, ErrNoPullRequest) ||
		errors.Is(err, ErrUnhandled)
}

type Event struct {
	Trigger string
	Action  string
	Owner   string
	Repo    string
	PR      int
	HeadSHA string
	Actor   string

	CommentBody string
	CommentID   int64
}

func FromEnv() (*Event, error) {
	path := os.Getenv("GITHUB_EVENT_PATH")
	if path == "" {
		return nil, errors.New("GITHUB_EVENT_PATH is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open event payload %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	ev, err := Parse(os.Getenv("GITHUB_EVENT_NAME"), f)
	if err != nil {
		return nil, fmt.Errorf("event payload %s: %w", path, err)
	}
	return ev, nil
}

type payload struct {
	Action     string `json:"action"`
	Number     int    `json:"number"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender *struct {
		Login string `json:"login"`
	} `json:"sender"`
	Issue *struct {
		Number      int `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	PullRequest *struct {
		Number int `json:"number"`
		Head   *struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	CheckSuite *struct {
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
}

func Parse(trigger string, r io.Reader) (*Event, error) {
	if trigger == "" {
		return nil, errors.New("GITHUB_EVENT_NAME is not set")
	}

	var p payload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("decode payload for trigger %s: %w", trigger, err)
	}

	if p.Repository == nil || p.Repository.FullName == "" {
		return nil, fmt.Errorf("payload for trigger %s has no repository.full_name", trigger)
	}
	owner, repo, ok := strings.Cut(p.Repository.FullName, "/")
	if !ok || owner == "" || repo == "" {
		return nil, fmt.Errorf("repository.full_name %q is not owner/name", p.Repository.FullName)
	}

	ev := &Event{Trigger: trigger, Action: p.Action, Owner: owner, Repo: repo}
	if p.Sender != nil {
		ev.Actor = p.Sender.Login
	}

	var err error
	switch trigger {
	case TriggerIssueComment:
		err = parseIssueComment(&p, ev)
	case TriggerPullRequest:
		err = parsePullRequest(&p, ev)
	case TriggerPullRequestReview:
		err = parsePullRequestReview(&p, ev)
	case TriggerCheckSuite:
		err = parseCheckSuite(&p, ev)
	default:
		err = fmt.Errorf("%w: trigger %s", ErrUnhandled, trigger)
	}
	if err != nil {
		return nil, err
	}
	if ev.PR <= 0 {
		return nil, fmt.Errorf("payload for trigger %s has no pull request number", trigger)
	}
	return ev, nil
}

func parseIssueComment(p *payload, ev *Event) error {
	if p.Action != "created" {
		return fmt.Errorf("%w: %s %s", ErrUnhandled, ev.Trigger, p.Action)
	}
	if p.Issue == nil {
		return errors.New("issue_comment payload has no issue")
	}
	if p.Issue.PullRequest == nil {
		return ErrNotPullRequest
	}
	if p.Comment == nil {
		return errors.New("issue_comment payload has no comment")
	}
	ev.PR = p.Issue.Number
	ev.CommentBody = p.Comment.Body
	ev.CommentID = p.Comment.ID
	return nil
}

func parsePullRequest(p *payload, ev *Event) error {
	switch p.Action {
	case "synchronize", "reopened", "closed":
	default:
		return fmt.Errorf("%w: %s %s", ErrUnhandled, ev.Trigger, p.Action)
	}
	if p.PullRequest == nil {
		return errors.New("pull_request payload has no pull_request")
	}
	ev.PR = p.PullRequest.Number
	if ev.PR == 0 {
		ev.PR = p.Number
	}
	setHeadSHA(p, ev)
	return nil
}

func parsePullRequestReview(p *payload, ev *Event) error {
	if p.Action != "submitted" {
		return fmt.Errorf("%w: %s %s", ErrUnhandled, ev.Trigger, p.Action)
	}
	if p.PullRequest == nil {
		return errors.New("pull_request_review payload has no pull_request")
	}
	ev.PR = p.PullRequest.Number
	setHeadSHA(p, ev)
	return nil
}

func parseCheckSuite(p *payload, ev *Event) error {
	if p.Action != "completed" {
		return fmt.Errorf("%w: %s %s", ErrUnhandled, ev.Trigger, p.Action)
	}
	if p.CheckSuite == nil {
		return errors.New("check_suite payload has no check_suite")
	}
	if len(p.CheckSuite.PullRequests) == 0 {
		return ErrNoPullRequest
	}
	ev.PR = p.CheckSuite.PullRequests[0].Number
	ev.HeadSHA = p.CheckSuite.HeadSHA
	return nil
}

func setHeadSHA(p *payload, ev *Event) {
	if p.PullRequest != nil && p.PullRequest.Head != nil {
		ev.HeadSHA = p.PullRequest.Head.SHA
	}
}
