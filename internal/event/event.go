// Package event parses the JSON payload that the Actions runtime leaves at
// GITHUB_EVENT_PATH into the handful of fields Talooner works from.
//
// Two kinds of failure come out of here and they are not the same. A malformed
// or unreadable payload is an error: the run is broken and should say so. An
// event Talooner deliberately does not serve — a comment on a plain issue, a
// check suite belonging to no PR, `pull_request opened` — is a skip: exit 0, no
// API calls, a skipped job rather than a red X on someone's PR. Skip reports
// which is which.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The triggers Talooner handles. `pull_request opened` is deliberately absent:
// the bot waits to be asked (architecture.md, "Invocation").
const (
	TriggerIssueComment      = "issue_comment"
	TriggerPullRequest       = "pull_request"
	TriggerPullRequestReview = "pull_request_review"
	TriggerCheckSuite        = "check_suite"
)

var (
	// ErrNotPullRequest is an issue_comment on an issue that is not a PR.
	ErrNotPullRequest = errors.New("comment is not on a pull request")
	// ErrNoPullRequest is an event that carries no PR to act on, such as a
	// check suite for a push to a branch with no open PR.
	ErrNoPullRequest = errors.New("event carries no pull request")
	// ErrUnhandled is a trigger or an action Talooner does not serve.
	ErrUnhandled = errors.New("unhandled event")
)

// Skip reports whether err means there is nothing to do, as opposed to
// something being wrong. Callers exit 0 on true.
func Skip(err error) bool {
	return errors.Is(err, ErrNotPullRequest) ||
		errors.Is(err, ErrNoPullRequest) ||
		errors.Is(err, ErrUnhandled)
}

// Event is what the rest of the bot sees of a GitHub event.
type Event struct {
	Trigger string // GITHUB_EVENT_NAME, one of the Trigger* constants
	Action  string // the payload's "action", e.g. created, synchronize, closed
	Owner   string
	Repo    string
	PR      int
	// HeadSHA is empty when the payload does not carry one — issue_comment
	// never does. Fetch it from the API rather than assuming it is set.
	HeadSHA string
	Actor   string // who caused the event

	// CommentBody and CommentID are set for issue_comment only. The command
	// parser reads the body; the comment id is what a reply threads onto.
	CommentBody string
	CommentID   int64
}

// FromEnv reads GITHUB_EVENT_NAME and GITHUB_EVENT_PATH and parses the payload.
func FromEnv() (*Event, error) {
	path := os.Getenv("GITHUB_EVENT_PATH")
	if path == "" {
		return nil, errors.New("GITHUB_EVENT_PATH is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open event payload %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	ev, err := Parse(os.Getenv("GITHUB_EVENT_NAME"), f)
	if err != nil {
		return nil, fmt.Errorf("event payload %s: %w", path, err)
	}
	return ev, nil
}

// payload is the subset of the webhook shapes that Talooner reads. Every nested
// object that is absent from some trigger is a pointer, so a missing one is a
// nil check and not a zero value that reads like real data.
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

// Parse reads a webhook payload for the named trigger.
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
	// The one marker distinguishing a PR comment from an issue comment: GitHub
	// sets issue.pull_request on the former and omits it on the latter.
	if p.Issue.PullRequest == nil {
		return ErrNotPullRequest
	}
	if p.Comment == nil {
		return errors.New("issue_comment payload has no comment")
	}
	ev.PR = p.Issue.Number
	ev.CommentBody = p.Comment.Body
	ev.CommentID = p.Comment.ID
	// No head sha in this payload; the caller fetches the PR for it.
	return nil
}

func parsePullRequest(p *payload, ev *Event) error {
	switch p.Action {
	case "synchronize", "reopened", "closed":
	default:
		// Includes "opened": Talooner waits to be asked.
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
	// A suite for a push to a branch with no open PR carries an empty list.
	if len(p.CheckSuite.PullRequests) == 0 {
		return ErrNoPullRequest
	}
	ev.PR = p.CheckSuite.PullRequests[0].Number
	ev.HeadSHA = p.CheckSuite.HeadSHA
	return nil
}

// setHeadSHA fills HeadSHA when the payload carries one. Some shapes do not —
// leaving it empty is the contract, reading through a nil head is not.
func setHeadSHA(p *payload, ev *Event) {
	if p.PullRequest != nil && p.PullRequest.Head != nil {
		ev.HeadSHA = p.PullRequest.Head.SHA
	}
}
