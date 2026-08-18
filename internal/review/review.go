// Package review turns a decision into the GitHub review Talooner leaves on a
// pull request: APPROVE when approve fired, REQUEST_CHANGES when block did, and
// nothing standing when neither did.
//
// The third case is the one this package exists for. Facts retract and GitHub
// reviews do not, so a PR that was approved and has since grown past 500 lines
// has to have that approval *dismissed* on the next run — otherwise the bot's
// last opinion outlives the facts it was computed from (actions.md,
// "Reversibility"). Retraction is the absence of an action, which no executor
// can be handed, so the verb's executor performs the positive half and Sync
// performs both: the run calls it whether or not either verb fired.
//
// Two more things are deliberate:
//
//   - The review is one value for the whole action set, like the check run.
//     approve and block can both fire on an unresolved tie, and the review says
//     what the check run says — block wins — rather than submitting an approval
//     and dismissing it moments later.
//   - No plugin-supplied text reaches the body. A review body cannot be edited
//     to a resolved state the way a sticky comment can, and the findings are
//     already in that comment; keeping the body to Talooner's own words means
//     there is nothing here for a fork PR's title to forge (comment.escape).
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

// Marker identifies Talooner's own reviews in the listing. Like every other
// marker its spelling is a compatibility surface: changing it orphans every
// review earlier versions left, and an orphaned approval is one nothing will
// ever dismiss.
func Marker() string { return comment.Marker(comment.TopicVerdict) }

// Verdict is the review event an action set calls for, or "" when it calls for
// none. Both verbs firing is the plugin's unresolved tie, and block wins here
// for the same last-resort reason it wins in the check run: the review has to
// have one value, and the warning telling the maintainer to disambiguate is the
// real product (actions.md, "Conflict resolution happens plugin-side").
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

// Submitter is the one call this package makes against GitHub.
type Submitter interface {
	SyncReview(ctx context.Context, owner, repo string, number int, rv github.Review) (int64, error)
}

// Writer performs one run's review. It is built with the verdict already
// decided, so the executor and the retraction pass cannot disagree about what
// the review should say.
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

// New returns the writer for one run. event comes from Verdict; an empty one
// means this run has no verdict, so Sync retracts whatever is standing.
func New(gh Submitter, owner, repo string, number int, headSHA, event string, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Writer{gh: gh, owner: owner, repo: repo, number: number, headSHA: headSHA, event: event, log: log}
}

// Execute performs the review for approve and block. Both verbs map to the same
// writer and the same single write: `do approve "pr"` and `do block "pr.merge"`
// firing together is one review, not two.
func (w *Writer) Execute(ctx context.Context, a action.Action) error {
	if a.Verb != action.VerbApprove && a.Verb != action.VerbBlock {
		return fmt.Errorf("%w: the review writer does not perform %s", action.ErrUnknownVerb, a.Verb)
	}
	return w.Sync(ctx)
}

// Sync makes the standing review match this run's verdict, once per run. The
// run calls it after the registry, so a decision with neither verb in it still
// dismisses the review the last one left.
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

// Body is what the review says. It is short on purpose: the findings are in the
// sticky comment, and this is the machine-readable half's human sentence.
func Body(event, sha string) string {
	var b strings.Builder
	switch event {
	case github.ReviewApprove:
		b.WriteString("### Talooner approves\n\n")
		b.WriteString("The rules in `.github/talooner/rules.tln` all pass on this pull request.\n\n")
		// Saying so is the point: a maintainer who reads this as a review done
		// has been misled by a linter with an opinion.
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

// DismissMessage is what GitHub shows in place of the review Talooner
// retracted. It names the sha that changed the answer, because "dismissed by
// talooner" with no reason is the version a maintainer has to go digging about.
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
