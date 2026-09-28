// Package host defines the git-host-neutral vocabulary Talooner's facts,
// review, assignment and command layers are built on, plus the interfaces a
// concrete backend (internal/host/github, and eventually internal/host/gitlab)
// must satisfy to plug into internal/run.
package host

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("not found")

const DiffMaxBytes = 1 << 20

const (
	ReviewApprove        = "APPROVE"
	ReviewRequestChanges = "REQUEST_CHANGES"
)

const (
	StateApproved         = "APPROVED"
	StateChangesRequested = "CHANGES_REQUESTED"
)

const (
	ConclusionSuccess = "success"
	ConclusionFailure = "failure"
	ConclusionNeutral = "neutral"
)

const (
	LevelNotice  = "notice"
	LevelWarning = "warning"
	LevelFailure = "failure"
)

type PullRequest struct {
	Number       int
	HeadSHA      string
	BaseSHA      string
	HeadRef      string
	BaseRef      string
	Author       string
	Title        string
	Body         string
	State        string
	Draft        bool
	Merged       bool
	IsFork       bool
	Additions    int
	Deletions    int
	ChangedFiles int
	Commits      int
	Labels       []string
	Mergeable    *bool
	Assignees    []string
	Requested    Reviewers
}

type FileStat struct {
	Path      string
	Additions int
	Deletions int
}

type Reviewers struct {
	Users []string
	Teams []string
}

type Review struct {
	Marker         string
	Event          string
	Body           string
	CommitID       string
	DismissMessage string
}

type ReviewReport struct {
	ID       int64
	Login    string
	Bot      bool
	State    string
	CommitID string
}

type StickyComment struct {
	Marker   string
	Body     string
	EditOnly bool
}

type Annotation struct {
	Path      string
	StartLine int
	EndLine   int
	Level     string
	Title     string
	Message   string
}

type CheckRun struct {
	Name        string
	HeadSHA     string
	Conclusion  string
	Title       string
	Summary     string
	Text        string
	DetailsURL  string
	Annotations []Annotation
}

type CheckRunReport struct {
	Name       string
	Status     string
	Conclusion string
}

type CommitStatus struct {
	Context string
	State   string
}

type Checks struct {
	Runs     []CheckRunReport
	Statuses []CommitStatus
}

func (c Checks) Pending() bool {
	for _, r := range c.Runs {
		if r.Status == "queued" || r.Status == "in_progress" {
			return true
		}
	}
	for _, s := range c.Statuses {
		if s.State == "pending" {
			return true
		}
	}
	return false
}

// Source is the read side: everything facts.PR needs to turn a pull/merge
// request into facts.
type Source interface {
	ResolveMergeable(ctx context.Context, owner, repo string, number int) (*PullRequest, error)
	ChangedFileStats(ctx context.Context, owner, repo string, number int) ([]FileStat, error)
	CommitChecks(ctx context.Context, owner, repo, headSHA string) (Checks, error)
	Diff(ctx context.Context, owner, repo string, number, maxBytes int) (string, bool, error)
	PullRequestReviews(ctx context.Context, owner, repo string, number int) ([]ReviewReport, error)
	LastToucher(ctx context.Context, owner, repo, baseSHA string, paths []string) (string, error)
}

// Submitter is the review-writing side: approve/request-changes as a single
// standing review that gets synced (and dismissed) as the verdict changes.
type Submitter interface {
	SyncReview(ctx context.Context, owner, repo string, number int, rv Review) (int64, error)
}

// Writer is the assignment-writing side: assignees, review requests, and the
// sticky comment the assignment ledger is persisted in.
type Writer interface {
	CommentBody(ctx context.Context, owner, repo string, number int, marker string) (string, error)
	UpsertComment(ctx context.Context, owner, repo string, number int, s StickyComment) (int64, error)
	AddAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error)
	RemoveAssignees(ctx context.Context, owner, repo string, number int, logins []string) ([]string, error)
	RequestReviewers(ctx context.Context, owner, repo string, number int, users, teams []string) (Reviewers, error)
	RemoveReviewRequests(ctx context.Context, owner, repo string, number int, users, teams []string) (Reviewers, error)
}

// PermissionChecker authorizes a comment command against repo write access.
type PermissionChecker interface {
	HasWriteAccess(ctx context.Context, owner, repo, login string) (bool, error)
}

// Host is the full surface internal/run.Runner needs from a git host backend.
type Host interface {
	Source
	Submitter
	Writer
	PermissionChecker
	PullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error)
	FileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	CreateComment(ctx context.Context, owner, repo string, number int, body string) (int64, error)
	UpsertCheckRun(ctx context.Context, owner, repo string, cr CheckRun) (int64, error)
}
