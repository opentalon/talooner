package review

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/comment"
	"github.com/opentalon/talooner/internal/github"
)

func Marker() string { return comment.Marker(comment.TopicVerdict) }

func Verdict(actions []action.Action) string {
	var approved bool
	for _, a := range actions {
		switch a.Verb {
		case action.VerbBlock:
			return github.ReviewRequestChanges
		case action.VerbApprove:
			approved = true
		}
	}
	if approved {
		return github.ReviewApprove
	}
	return ""
}

type Submitter interface {
	SyncReview(ctx context.Context, owner, repo string, number int, rv github.Review) (int64, error)
}

type Writer struct {
	gh      Submitter
	owner   string
	repo    string
	number  int
	headSHA string
	event   string
	log     *slog.Logger
	done    bool
}

func New(gh Submitter, owner, repo string, number int, headSHA, event string, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Writer{gh: gh, owner: owner, repo: repo, number: number, headSHA: headSHA, event: event, log: log}
}

func (w *Writer) Execute(ctx context.Context, a action.Action) error {
	if a.Verb != action.VerbApprove && a.Verb != action.VerbBlock {
		return fmt.Errorf("%w: the review writer does not perform %s", action.ErrUnknownVerb, a.Verb)
	}
	return w.Sync(ctx)
}

func (w *Writer) Sync(ctx context.Context) error {
	if w.done {
		return nil
	}
	w.done = true

	id, err := w.gh.SyncReview(ctx, w.owner, w.repo, w.number, github.Review{
		Marker:         Marker(),
		Event:          w.event,
		Body:           Body(w.event, w.headSHA),
		CommitID:       w.headSHA,
		DismissMessage: DismissMessage(w.headSHA),
	})
	if err != nil {
		return fmt.Errorf("write the review on %s/%s#%d: %w", w.owner, w.repo, w.number, err)
	}
	w.log.Info("review synced", "repo", w.owner+"/"+w.repo, "pr", w.number,
		"sha", w.headSHA, "event", w.event, "review", id)
	return nil
}

func Body(event, sha string) string {
	var b strings.Builder
	switch event {
	case github.ReviewApprove:
		b.WriteString("### Talooner approves\n\n")
		b.WriteString("The rules in `.github/talooner/rules.tln` all pass on this pull request.\n\n")
		b.WriteString("This approval is advisory. It is a pre-pass before human review, not a substitute " +
			"for one, and it does not satisfy a branch protection rule that requires approvals from people.\n")
	case github.ReviewRequestChanges:
		b.WriteString("### Talooner requests changes\n\n")
		b.WriteString("A rule in `.github/talooner/rules.tln` blocked this pull request. " +
			"The findings are in Talooner's review comment on this pull request, and in the `talooner` check run.\n\n")
		b.WriteString("Whether this blocks the merge is the repository's branch protection to decide; " +
			"Talooner has no merge rights either way.\n")
	default:
		return ""
	}
	b.WriteString(footer(sha))
	return b.String()
}

func DismissMessage(sha string) string {
	if sha == "" {
		return "Talooner re-evaluated this pull request and this verdict no longer holds."
	}
	return fmt.Sprintf("Talooner re-evaluated this pull request at %s and this verdict no longer holds.", short(sha))
}

func footer(sha string) string {
	if sha == "" {
		return "\n<sub>Talooner dismisses this review when the rules stop saying it.</sub>\n"
	}
	return fmt.Sprintf("\n<sub>Talooner reviewed <code>%s</code>. This review is dismissed when the rules stop saying it.</sub>\n",
		short(sha))
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
