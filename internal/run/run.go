// Package run is the spine of a Talooner run: one GitHub event in, one
// evaluation out.
//
// It is the walking skeleton of architecture.md, "Evaluate" — subscribed? →
// load the ruleset from the base branch → extract facts → evaluate_pr → report
// — with the pieces the later tickets own left as one log line each. What is
// written back to GitHub so far is the talooner check run, the sticky review
// comment, the approve/block review, and the assignees and review requests; the
// notifications are D6, the full .github/talooner loader E1, and fork plan mode
// E2.
//
// Once a run knows the head sha it owes that sha a check run, including when it
// breaks. A failure of Talooner's own is written as neutral, never failure: a
// repo that marked the check required must not be blocked by the bot's bugs
// (actions.md, "Check run"). The job itself still exits non-zero, so the
// failure is visible where it belongs.
//
// Two orderings in here are security controls rather than style:
//
//   - the command is parsed before anything is dialled and the commander is
//     authorized before anything is said, so an unauthorised account cannot make
//     the bot answer it (command.Authorize);
//   - the ruleset is read from the PR's base branch at its own ref, never from
//     the head, because a fork PR's .github/talooner is attacker-editable.
//
// Every "nothing to do" outcome returns nil. An unsubscribed PR, a comment with
// no command, a repo with no ruleset — all of those are a skipped job, not a red
// X on someone's PR. An error out of Run means the run itself is broken.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/assignment"
	"github.com/opentalon/talooner/internal/check"
	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/command"
	"github.com/opentalon/talooner/internal/comment"
	"github.com/opentalon/talooner/internal/event"
	"github.com/opentalon/talooner/internal/facts"
	"github.com/opentalon/talooner/internal/github"
	"github.com/opentalon/talooner/internal/review"
)

// RulesetPath is the tenant ruleset, read from the base branch. The rest of
// .github/talooner/ — config.yaml, modules.yaml, teams.yaml — arrives with E1.
const RulesetPath = ".github/talooner/rules.tln"

// Runner is one run's dependencies, already built and connected.
type Runner struct {
	Event   *event.Event
	GitHub  *github.Client
	Cluster *cluster.Client
	// Handle is the string Talooner answers to in a comment. Empty means
	// command.DefaultHandle; the per-repo override is config, so it lands with E1.
	Handle string
	Log    *slog.Logger
}

// Run performs one evaluation.
func Run(ctx context.Context, r Runner) error {
	if r.Event == nil {
		return errors.New("run has no event")
	}
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	if r.Handle == "" {
		r.Handle = command.DefaultHandle
	}
	ev := r.Event
	repo := ev.Owner + "/" + ev.Repo // the plugin's scope key is {repo}#{number}

	if ev.Trigger == event.TriggerIssueComment {
		cmd, err := r.gate(ctx)
		if err != nil || cmd == nil {
			return err // handled: either nothing to do, or a real failure
		}
		switch cmd.Verb {
		case command.VerbReview:
			// Subscribe first: a review that fails halfway still leaves the PR
			// watched, which is what the commander asked for.
			if _, err := r.Cluster.SetSubscription(ctx, repo, ev.PR, true); err != nil {
				return fmt.Errorf("subscribe %s#%d: %w", repo, ev.PR, err)
			}
		case command.VerbStop:
			if _, err := r.Cluster.SetSubscription(ctx, repo, ev.PR, false); err != nil {
				return fmt.Errorf("unsubscribe %s#%d: %w", repo, ev.PR, err)
			}
			r.Log.Info("unsubscribed", "repo", repo, "pr", ev.PR, "actor", ev.Actor)
			return nil
		default:
			// /why is explain_pr (its own ticket) and /plan is the head-branch
			// dry run (E2). Both are parsed and authorized already, so this is
			// the one place left to add them.
			r.Log.Warn("command is not wired up yet", "verb", cmd.Verb, "repo", repo, "pr", ev.PR)
			return nil
		}
	} else {
		// Every trigger except a comment runs only to serve a PR somebody
		// already invoked Talooner on.
		if ev.Trigger == event.TriggerPullRequest && ev.Action == "closed" {
			if _, err := r.Cluster.SetSubscription(ctx, repo, ev.PR, false); err != nil {
				return fmt.Errorf("unsubscribe closed %s#%d: %w", repo, ev.PR, err)
			}
			r.Log.Info("pull request closed, unsubscribed", "repo", repo, "pr", ev.PR)
			return nil
		}
		subscribed, err := r.Cluster.IsSubscribed(ctx, repo, ev.PR)
		if err != nil {
			return fmt.Errorf("check subscription of %s#%d: %w", repo, ev.PR, err)
		}
		if !subscribed {
			r.Log.Info("pull request is not subscribed, nothing to do",
				"repo", repo, "pr", ev.PR, "trigger", ev.Trigger)
			return nil
		}
	}

	// The PR itself, for the head sha an issue_comment payload never carries and
	// for the base ref the ruleset is read from. facts.PR fetches it again; one
	// extra GET is cheaper than an extractor that takes what it needs as an
	// argument and can be handed something else.
	pr, err := r.GitHub.PullRequest(ctx, ev.Owner, ev.Repo, ev.PR)
	if err != nil {
		return fmt.Errorf("fetch %s#%d: %w", repo, ev.PR, err)
	}
	if ev.HeadSHA != "" && ev.HeadSHA != pr.HeadSHA {
		// The PR moved between the event and this run. The later run will
		// re-evaluate at the newer sha, so this one stops rather than writing a
		// verdict for a sha nobody is looking at.
		r.Log.Info("head sha moved since the event, leaving it to the newer run",
			"repo", repo, "pr", ev.PR, "event_sha", ev.HeadSHA, "current_sha", pr.HeadSHA)
		return nil
	}
	if pr.BaseRef == "" {
		return fmt.Errorf("%s#%d came back with no base ref", repo, ev.PR)
	}

	// From here on there is a head sha to write a verdict against, so every
	// failure owes this PR a check run. failOpen writes the neutral one: a run
	// that died halfway must not leave the previous run's success standing, and
	// must not turn a Talooner bug into a merge blocker either.
	if err := r.evaluate(ctx, repo, pr); err != nil {
		return r.failOpen(ctx, repo, pr, err)
	}
	return nil
}

// evaluate is the part of a run that owes a check run: load the ruleset, extract
// the facts, ask the cluster, write the verdict.
func (r Runner) evaluate(ctx context.Context, repo string, pr *github.PullRequest) error {
	ev := r.Event

	ruleset, err := r.GitHub.FileContent(ctx, ev.Owner, ev.Repo, RulesetPath, pr.BaseRef)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			// Not an error, and deliberately not a check run either: a repo that
			// has not onboarded gets no talooner check at all, rather than a
			// neutral one implying the bot tried and failed. E1 turns this into
			// one comment saying so.
			r.Log.Info("no ruleset on the base branch, nothing to evaluate",
				"repo", repo, "pr", ev.PR, "path", RulesetPath, "ref", pr.BaseRef)
			return nil
		}
		return fmt.Errorf("load ruleset: %w", err)
	}

	set, err := facts.PR(ctx, r.GitHub, ev.Owner, ev.Repo, ev.PR)
	if err != nil {
		return err // already names the repo and PR, and is never partial
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:    repo,
		PR:      ev.PR,
		HeadSHA: pr.HeadSHA,
		Facts:   set,
		Ruleset: string(ruleset),
		// Execute mode: the ruleset came from the base branch, so it is the
		// maintainers'. Plan mode is E2's head-branch path.
		Mode: cluster.ModeExecute,
	})
	if err != nil {
		evalErr := fmt.Errorf("evaluate %s#%d: %w", repo, ev.PR, err)
		if !errors.Is(err, cluster.ErrAction) {
			return evalErr // transport, not the ruleset: nothing to annotate
		}
		// The plugin ran and refused. Usually that is a ruleset that will not
		// compile, and evaluate_pr says so without a position, so the line
		// numbers come from a second call.
		return r.rulesetBroken(ctx, repo, pr, string(ruleset), evalErr)
	}

	return r.report(ctx, repo, pr, resp)
}

// gate parses the command in a comment and checks the commander's access. It
// returns (nil, nil) when there is nothing to do — the caller exits 0 without
// saying anything, which is the whole point: a reply to an unauthorised account
// tells it the bot is installed and hands it a way to make the bot post.
func (r Runner) gate(ctx context.Context) (*command.Command, error) {
	ev := r.Event
	cmd, parseErr := command.Parse(r.Handle, ev.CommentBody)
	if errors.Is(parseErr, command.ErrNoCommand) {
		return nil, nil // not addressed to us: no API calls at all
	}

	// Anything else, a parse error included, is answered only after the
	// commander is shown to have write access.
	if err := command.Authorize(ctx, r.GitHub, ev.Owner, ev.Repo, ev.Actor); err != nil {
		if errors.Is(err, command.ErrNotAuthorized) {
			r.Log.Info("ignoring command from an account without write access",
				"repo", ev.Owner+"/"+ev.Repo, "pr", ev.PR, "actor", ev.Actor)
			return nil, nil
		}
		return nil, err // an API failure must fail the run, not drop the command
	}

	if parseErr != nil {
		// An authorized user typing something wrong is worth exactly one reply,
		// and exactly one comment however many times they retry: the usage text
		// is its own sticky topic, so it is edited rather than piled up, and it
		// never overwrites the verdict.
		r.Log.Warn("command not understood", "actor", ev.Actor, "err", parseErr)
		if err := r.sticky(ctx, comment.TopicUsage, comment.Usage(command.Usage(r.Handle)), false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return cmd, nil
}

// report writes the decision: the sticky review comment, the actions
// themselves, and the talooner check run. The one part still missing is notify,
// which is D6.
//
// The action set is decoded before anything is written, so a verdict carrying a
// verb this build has never heard of fails the run instead of being written as
// a check run that describes half of it.
func (r Runner) report(ctx context.Context, repo string, pr *github.PullRequest, resp *taloonerpb.EvaluatePrResponse) error {
	warnings := make([]check.Warning, 0, len(resp.GetWarnings()))
	for _, w := range resp.GetWarnings() {
		r.Log.Warn("plugin warning", "code", w.GetCode(), "message", w.GetMessage())
		warnings = append(warnings, check.Warning{Code: w.GetCode(), Message: w.GetMessage()})
	}
	summary := resp.GetExplain().GetSummary()
	if summary != "" {
		r.Log.Info("decision", "repo", repo, "pr", r.Event.PR, "summary", summary)
	}

	actions, err := action.FromProtos(resp.GetActions())
	if err != nil {
		return fmt.Errorf("decode the decision for %s#%d: %w", repo, r.Event.PR, err)
	}
	for _, a := range actions {
		r.Log.Info("action", "repo", repo, "pr", r.Event.PR, "verb", a.Verb, "plan", action.Describe(a))
	}

	// The registry is built and checked before anything is written. A verdict
	// carrying a verb nothing here performs — notify until D6 — fails the run
	// rather than being carried out in part: an action with no executor is a hard
	// error and never a no-op (actions.md), and half a verdict published is the
	// one outcome worse than none.
	//
	// Building the assignment syncer resolves every assign and require argument,
	// so a ruleset naming a team that maps to nobody fails here, before the
	// sticky comment goes out, rather than halfway through the writes.
	rev := review.New(r.GitHub, r.Event.Owner, r.Event.Repo, r.Event.PR,
		pr.HeadSHA, review.Verdict(actions), r.Log)
	asg, err := assignment.New(r.GitHub, r.Event.Owner, r.Event.Repo, r.Event.PR, pr, actions, r.Log)
	if err != nil {
		return fmt.Errorf("cannot carry out the decision for %s#%d: %w", repo, r.Event.PR, err)
	}
	registry := action.Registry{
		action.VerbApprove: rev,
		action.VerbBlock:   rev,
		action.VerbAssign:  asg,
		action.VerbRequire: asg,
		action.VerbComment: action.Derived("written as the sticky review comment from the whole action set"),
		action.VerbEmit:    action.Derived("asserted plugin-side; emit has no GitHub effect"),
	}
	if err := registry.Validate(actions); err != nil {
		return fmt.Errorf("cannot carry out the decision for %s#%d: %w", repo, r.Event.PR, err)
	}

	// The sticky comment goes first, and the check run last. Both orderings are
	// deliberate: the check run is the run's "everything worked" marker, so a
	// comment that could not be written leaves no verdict standing and failOpen
	// writes the neutral check over whatever the previous run left. The reverse
	// order would have a failed comment write overwrite this run's own correct
	// verdict with a neutral one.
	if err := r.reviewComment(ctx, repo, pr, actions, warnings, summary); err != nil {
		return err
	}

	// Then the actions themselves. approve and block are one GitHub review
	// between them, and Sync runs whether or not either fired: a decision with
	// neither in it has to dismiss the review the last one left, and retraction
	// is the absence of an action, which no executor is ever handed.
	if err := registry.Execute(ctx, actions); err != nil {
		return fmt.Errorf("carry out the decision for %s#%d: %w", repo, r.Event.PR, err)
	}
	if err := rev.Sync(ctx); err != nil {
		return err
	}
	// Same shape, and for the same reason: a decision with no assign and no
	// require in it has to take back the assignees and review requests the last
	// one added, and there is no action to hang that on.
	if err := asg.Sync(ctx); err != nil {
		return fmt.Errorf("sync the assignments for %s#%d: %w", repo, r.Event.PR, err)
	}

	// The check run is derived from the whole action set rather than performed
	// by one verb's executor: it is one value for the decision, and it is
	// written even for a verb whose GitHub half is not built yet (notify, D6). The
	// review comment is the same shape of thing — `do comment` firing three
	// times is one comment with three findings, not three comments. It goes
	// last, as the run's "everything worked" marker.
	cr := check.Decision(actions, warnings, summary)
	cr.HeadSHA = pr.HeadSHA
	if _, err := r.GitHub.UpsertCheckRun(ctx, r.Event.Owner, r.Event.Repo, cr); err != nil {
		return fmt.Errorf("write the check run for %s#%d: %w", repo, r.Event.PR, err)
	}
	r.Log.Info("check run written", "repo", repo, "pr", r.Event.PR,
		"sha", pr.HeadSHA, "conclusion", cr.Conclusion, "actions", len(actions))
	return nil
}

// reviewComment writes the verdict as the sticky review comment, or retires it.
// The GitHub review itself is the review package's; this is the prose half.
//
// A run with no findings and no warnings posts nothing: the check run already
// says the rules passed, and a comment costs every watcher an email. But if a
// previous run left findings on this PR, they are now wrong, so the comment is
// edited to its resolved state rather than left standing — and never deleted,
// because the discussion under it is somebody's (actions.md, "Reversibility").
func (r Runner) reviewComment(ctx context.Context, repo string, pr *github.PullRequest,
	actions []action.Action, warnings []check.Warning, summary string,
) error {
	body, editOnly := comment.Review(actions, warnings, summary, pr.HeadSHA), false
	if comment.Empty(actions, warnings) {
		body, editOnly = comment.Resolved(pr.HeadSHA), true
	}
	if err := r.sticky(ctx, comment.TopicReview, body, editOnly); err != nil {
		return fmt.Errorf("write the review comment for %s#%d: %w", repo, r.Event.PR, err)
	}
	return nil
}

// sticky writes one topic's comment on the event's PR. editOnly writes nothing
// when the topic has no comment yet.
func (r Runner) sticky(ctx context.Context, topic, body string, editOnly bool) error {
	ev := r.Event
	id, err := r.GitHub.UpsertComment(ctx, ev.Owner, ev.Repo, ev.PR, github.StickyComment{
		Marker:   comment.Marker(topic),
		Body:     body,
		EditOnly: editOnly,
	})
	if err != nil {
		return err
	}
	if id == 0 {
		return nil // nothing to retire, and nothing posted
	}
	r.Log.Info("sticky comment written",
		"repo", ev.Owner+"/"+ev.Repo, "pr", ev.PR, "topic", topic, "id", id)
	return nil
}

// rulesetBroken writes the neutral check run for a ruleset the plugin refused,
// with one annotation per compiler diagnostic pinned to its line. It returns the
// original failure, marked so failOpen does not write over the annotations with
// a blank neutral check.
func (r Runner) rulesetBroken(ctx context.Context, repo string, pr *github.PullRequest, ruleset string, cause error) error {
	var diags []check.Diagnostic
	resp, err := r.Cluster.ValidateRuleset(ctx, ruleset)
	if err != nil {
		// The refusal may not have been about the ruleset at all, or the cluster
		// may have gone away in between. Either way the check run is still owed,
		// just without positions.
		r.Log.Warn("cannot get ruleset diagnostics", "repo", repo, "pr", r.Event.PR, "err", err)
	} else {
		for _, d := range resp.GetDiagnostics() {
			if d.GetSeverity() != taloonerpb.Severity_SEVERITY_ERROR {
				continue
			}
			diags = append(diags, check.Diagnostic{
				Path:    RulesetPath,
				Line:    int(d.GetLine()),
				Column:  int(d.GetColumn()),
				Message: d.GetMessage(),
			})
		}
	}

	cr := check.Broken(cause.Error(), diags)
	cr.HeadSHA = pr.HeadSHA
	if _, err := r.GitHub.UpsertCheckRun(ctx, r.Event.Owner, r.Event.Repo, cr); err != nil {
		r.Log.Error("cannot write the neutral check run", "repo", repo, "pr", r.Event.PR, "err", err)
	}

	// The annotations pin the error to its line; this is the summary comment
	// actions.md pairs them with, and it takes over the review topic because it
	// is the current answer to the same question — the findings of the run
	// before this one no longer hold.
	if err := r.sticky(ctx, comment.TopicReview, comment.Broken(cause.Error(), pr.HeadSHA), false); err != nil {
		// The run is already failing with a better error than this one.
		r.Log.Error("cannot write the review comment", "repo", repo, "pr", r.Event.PR, "err", err)
	}
	return reported{cause}
}

// failOpen writes the neutral check run for a run that broke after it had a head
// sha, then returns the failure unchanged. The job still goes red — a broken run
// is worth a maintainer's attention — but the check the branch protection reads
// is neutral, and it replaces whatever the last run left at this sha.
func (r Runner) failOpen(ctx context.Context, repo string, pr *github.PullRequest, cause error) error {
	var already reported
	if errors.As(cause, &already) {
		return already.err // its check run is written, with better words on it
	}

	cr := check.Broken(cause.Error(), nil)
	cr.HeadSHA = pr.HeadSHA
	if _, err := r.GitHub.UpsertCheckRun(ctx, r.Event.Owner, r.Event.Repo, cr); err != nil {
		// Two failures, and the first one is the interesting one.
		r.Log.Error("cannot write the neutral check run", "repo", repo, "pr", r.Event.PR, "err", err)
	}
	return cause
}

// reported wraps a failure whose check run has already been written, so the
// generic handler does not overwrite a specific verdict with a vague one.
type reported struct{ err error }

func (r reported) Error() string { return r.err.Error() }
func (r reported) Unwrap() error { return r.err }

// Main is the action entry point: build everything from the environment, run,
// and turn the outcome into an exit code. 0 is success and every deliberate
// skip; 1 is a broken run.
func Main(ctx context.Context, log *slog.Logger) int {
	ev, err := event.FromEnv()
	if err != nil {
		if event.Skip(err) {
			log.Info("nothing to do for this event", "reason", err)
			return 0
		}
		log.Error("read event", "err", err)
		return 1
	}
	log = log.With("repo", ev.Owner+"/"+ev.Repo, "pr", ev.PR, "trigger", ev.Trigger)

	// The cluster is dialled before the GitHub client so the API key can be
	// registered with its redactor. The handshake happens even for a command
	// that turns out to be unauthorised, which costs one read-only call and
	// keeps every log path redacted.
	cl, err := cluster.DialFromEnv(ctx, cluster.WithLogger(log))
	if err != nil {
		// One line at the top of the run, naming which of the four it was.
		log.Error("cannot reach the cluster", "err", err)
		return 1
	}
	defer cl.Close() //nolint:errcheck // the run is over either way

	gh, err := github.NewFromEnv(github.WithLogger(log), github.WithSecrets(cl.APIKey()))
	if err != nil {
		log.Error("build github client", "err", err)
		return 1
	}

	if err := Run(ctx, Runner{
		Event:   ev,
		GitHub:  gh,
		Cluster: cl,
		Log:     log,
	}); err != nil {
		log.Error("run failed", "err", err)
		return 1
	}
	return 0
}
