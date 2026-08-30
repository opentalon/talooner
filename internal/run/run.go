// Package run is the spine of a Talooner run: one GitHub event in, one
// evaluation out.
//
// It is the walking skeleton of architecture.md, "Evaluate" — subscribed? →
// load the ruleset from the base branch → extract facts → evaluate_pr → report
// — with the pieces the later tickets own left as one log line each. What is
// written back to GitHub so far is the talooner check run, the sticky review
// comment, the approve/block review, and the assignees and review requests; the
// notifications are D6 and fork plan mode is E2.
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
// A fork PR's own head-branch ruleset is still read and evaluated (plan), but
// only in a mode with no write path at all, and its decision never reaches the
// base decision that report writes (E2, #21).
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
	"github.com/opentalon/talooner/internal/config"
	"github.com/opentalon/talooner/internal/event"
	"github.com/opentalon/talooner/internal/facts"
	"github.com/opentalon/talooner/internal/github"
	"github.com/opentalon/talooner/internal/review"
)

// RulesetPath is the tenant ruleset, read from the base branch alongside the
// rest of .github/talooner/: config.yaml, modules.yaml, teams.yaml, and
// CODEOWNERS (E1, #20). Only the per-repo handle override still lands later.
const RulesetPath = ".github/talooner/rules.tln"

// ConfigPath is the tenant's check-name patterns (C3), read from the base
// branch alongside the ruleset. Missing is an answer (no patterns, so no
// pr.tests_passing / pr.lint_passing); malformed is a tenant error that fails
// the run.
const ConfigPath = ".github/talooner/config.yaml"

// ModulePath is the tenant's module → docs/owner lookup table (C6), read from the
// base branch alongside the ruleset. Missing is an answer (no module.* facts, just
// module.touched_count = 0); malformed is a tenant error that fails the run.
const ModulePath = ".github/talooner/modules.yaml"

// TeamPath is the tenant's logical-team → GitHub-team map (C6), read from the
// base branch alongside the ruleset. Missing is an answer (require falls back to
// the path-derived target); malformed is a tenant error that fails the run.
const TeamPath = ".github/talooner/teams.yaml"

// ArchitecturePath is the tenant's code_unit layer override/extension
// (expert-review-system.md, Phase 1), read from the base branch alongside the
// ruleset. Missing is an answer (the built-in per-language conventions decide
// alone); malformed is a tenant error that fails the run.
const ArchitecturePath = ".github/talooner/architecture.yaml"

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
		case command.VerbWhy:
			// The issue_comment payload never carries a head sha (same reason the
			// shared flow below fetches the PR again): /why answers "the current
			// verdict", so it needs a fresh one, not the sha some earlier event
			// named.
			pr, err := r.GitHub.PullRequest(ctx, ev.Owner, ev.Repo, ev.PR)
			if err != nil {
				return fmt.Errorf("fetch %s#%d: %w", repo, ev.PR, err)
			}
			return r.why(ctx, repo, pr)
		case command.VerbPlan:
			// Same reason as VerbWhy: the issue_comment payload never carries a
			// head sha, and /plan answers "right now", so it needs a fresh PR.
			pr, err := r.GitHub.PullRequest(ctx, ev.Owner, ev.Repo, ev.PR)
			if err != nil {
				return fmt.Errorf("fetch %s#%d: %w", repo, ev.PR, err)
			}
			return r.planReply(ctx, repo, pr)
		default:
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
			// neutral one implying the bot tried and failed (D2). E1 says so in
			// one comment instead.
			r.Log.Info("no ruleset on the base branch, nothing to evaluate",
				"repo", repo, "pr", ev.PR, "path", RulesetPath, "ref", pr.BaseRef)
			if err := r.sticky(ctx, comment.TopicReview, comment.NoRuleset(RulesetPath, pr.HeadSHA), false); err != nil {
				return fmt.Errorf("write no-ruleset comment for %s#%d: %w", repo, ev.PR, err)
			}
			return nil
		}
		return fmt.Errorf("load ruleset: %w", err)
	}

	cfg, err := r.loadConfig(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err) // missing is handled inside; this is a malformed config
	}

	// CODEOWNERS feeds user.owner / user.owners (facts.md, "user.*"), read from
	// the base branch like the ruleset and config so a fork PR cannot name its
	// own owners. A repo with none is an answer, not an error.
	codeowners, err := r.loadCodeowners(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}

	// modules.yaml feeds module.* (facts.md, "module.*") and teams.yaml feeds the
	// require resolver (facts.md, "team.*"), both read from the base branch like
	// the ruleset and config so a fork PR cannot redefine what it touches or which
	// team answers a review request. A repo with neither is an answer, not an error.
	modules, err := r.loadModules(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}
	teams, err := r.loadTeams(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}
	// architecture.yaml feeds code.* (expert-review-system.md, Phase 1), read
	// from the base branch like modules.yaml so a fork PR cannot redefine its
	// own layer conventions. A repo with none is an answer, not an error — the
	// built-in per-language conventions decide alone.
	arch, err := r.loadArchitecture(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}

	set, units, err := facts.PR(ctx, r.GitHub, ev.Owner, ev.Repo, ev.PR, cfg.Checks, codeowners, modules, teams, arch)
	if err != nil {
		return err // already names the repo and PR, and is never partial
	}
	// code_units feeds llm_review (expert-review-system.md, Phase 2): each
	// touched, documented unit's doc, read from the base branch like every
	// other tenant config so a fork PR cannot rewrite what it is judged
	// against. A doc-loading problem warns rather than failing the run — the
	// LLM-review gate is additive, never a reason to withhold the rest of the
	// verdict.
	codeUnits, docWarnings, err := r.resolveCodeUnits(ctx, ev.Owner, ev.Repo, pr.BaseRef, units, arch)
	if err != nil {
		return fmt.Errorf("load code unit docs for %s#%d: %w", repo, ev.PR, err)
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:    repo,
		PR:      ev.PR,
		HeadSHA: pr.HeadSHA,
		Facts:   set,
		Ruleset: string(ruleset),
		// Execute mode: the ruleset came from the base branch, so it is the
		// maintainers'.
		Mode:      cluster.ModeExecute,
		CodeUnits: codeUnits,
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

	actions, err := action.FromProtos(resp.GetActions())
	if err != nil {
		return fmt.Errorf("decode the decision for %s#%d: %w", repo, ev.PR, err)
	}

	// The base decision above is the one that governs writes, full stop — it is
	// what report() below carries out. This is purely informational and never
	// allowed to affect that: a fork PR's own head-branch ruleset is evaluated
	// separately, in plan mode, so a contributor sees their rule change's effect
	// without being able to act on it (architecture.md, "Fork safety"). Best
	// effort — it must never turn a working base evaluation into a broken run.
	// codeUnits is already resolved above, so the plan call costs no extra API
	// requests to include it.
	if pr.IsFork {
		if err := r.plan(ctx, repo, pr, set, codeUnits, actions); err != nil {
			r.Log.Warn("fork plan comparison did not complete", "repo", repo, "pr", ev.PR, "err", err)
		}
	}

	return r.report(ctx, repo, pr, resp, actions, teams, docWarnings, len(codeUnits))
}

// plan evaluates a fork PR's own head-branch ruleset in plan mode — no writes
// are ever possible from it, ModePlan's response carries the decision in a
// field a caller cannot mistake for something to execute — and posts one
// comment showing how it would differ from the base decision that actually
// governs this run. base is the decision already reached from the base-branch
// ruleset; plan never changes it and is never merged into it.
//
// A repo with no ruleset of its own on the head branch is not a fork carrying
// anything to compare, so the comment (if any earlier run left one) is
// resolved rather than left describing a diff that no longer applies.
func (r Runner) plan(ctx context.Context, repo string, pr *github.PullRequest, set facts.Set, codeUnits []cluster.CodeUnit, base []action.Action) error {
	ev := r.Event

	headRuleset, err := r.GitHub.FileContent(ctx, ev.Owner, ev.Repo, RulesetPath, pr.HeadSHA)
	if errors.Is(err, github.ErrNotFound) {
		if err := r.sticky(ctx, comment.TopicPlan, comment.PlanResolved(pr.HeadSHA), true); err != nil {
			return fmt.Errorf("resolve stale plan comment for %s#%d: %w", repo, ev.PR, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load head ruleset: %w", err)
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:      repo,
		PR:        ev.PR,
		HeadSHA:   pr.HeadSHA,
		Facts:     set,
		Ruleset:   string(headRuleset),
		Mode:      cluster.ModePlan,
		CodeUnits: codeUnits,
	})
	if err != nil {
		return fmt.Errorf("evaluate head ruleset for %s#%d: %w", repo, ev.PR, err)
	}

	planned, err := action.FromProtos(resp.GetPlan())
	if err != nil {
		return fmt.Errorf("decode the plan for %s#%d: %w", repo, ev.PR, err)
	}

	added, removed := action.Diff(base, planned)
	body, editOnly := comment.Plan(added, removed, pr.HeadSHA), false
	if len(added) == 0 && len(removed) == 0 {
		body, editOnly = comment.PlanResolved(pr.HeadSHA), true
	}
	if err := r.sticky(ctx, comment.TopicPlan, body, editOnly); err != nil {
		return fmt.Errorf("write the plan comment for %s#%d: %w", repo, ev.PR, err)
	}
	return nil
}

// loadConfig reads the tenant's .github/talooner/config.yaml from the base
// branch and parses its check-name patterns. A missing file is an answer — the
// repo has no named checks, so pr.tests_passing / pr.lint_passing stay unset
// rather than being guessed. A present but unparseable file is a tenant error
// that fails the run, the same shape as a broken ruleset.
func (r Runner) loadConfig(ctx context.Context, owner, repo, ref string) (config.Config, error) {
	data, err := r.GitHub.FileContent(ctx, owner, repo, ConfigPath, ref)
	if errors.Is(err, github.ErrNotFound) {
		r.Log.Info("no config on the base branch, no check patterns",
			"repo", owner+"/"+repo, "path", ConfigPath, "ref", ref)
		return config.Config{}, nil
	}
	if err != nil {
		return config.Config{}, fmt.Errorf("load config %s from %s/%s@%s: %w", ConfigPath, owner, repo, ref, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return config.Config{}, fmt.Errorf("parse config %s from %s/%s@%s: %w", ConfigPath, owner, repo, ref, err)
	}
	return cfg, nil
}

// codeownersPaths are the locations GitHub consults for CODEOWNERS, in priority
// order (facts.md, "user.owner"). A repo may have none; that is an answer, not an
// error, so loadCodeowners returns nil content rather than failing.
var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// loadCodeowners reads the repo's CODEOWNERS from the base branch, trying each
// recognised location in priority order and returning the first that exists. A
// present-but-unreadable file is a tenant error that fails the run, the same
// shape as a broken ruleset; a repo with no CODEOWNERS at any location returns
// nil so user.owner / user.owners stay unset rather than guessed.
func (r Runner) loadCodeowners(ctx context.Context, owner, repo, ref string) ([]byte, error) {
	for _, p := range codeownersPaths {
		data, err := r.GitHub.FileContent(ctx, owner, repo, p, ref)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, github.ErrNotFound) {
			return nil, fmt.Errorf("load %s from %s/%s@%s: %w", p, owner, repo, ref, err)
		}
	}
	return nil, nil
}

// loadModules reads the tenant's .github/talooner/modules.yaml from the base
// branch and parses it into the module.* lookup entries. A missing file is an
// answer — the repo declared no modules, so module.touched_count reads 0 and the
// other module.* facts stay unset rather than being guessed. A present but
// unparseable file is a tenant error that fails the run, the same shape as a
// broken config.yaml.
func (r Runner) loadModules(ctx context.Context, owner, repo, ref string) ([]config.Module, error) {
	data, err := r.GitHub.FileContent(ctx, owner, repo, ModulePath, ref)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s from %s/%s@%s: %w", ModulePath, owner, repo, ref, err)
	}
	modules, err := config.ParseModules(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s from %s/%s@%s: %w", ModulePath, owner, repo, ref, err)
	}
	return modules, nil
}

// loadTeams reads the tenant's .github/talooner/teams.yaml from the base branch
// and parses the logical-team → GitHub-team map. A missing file is an answer —
// require targets fall back to the path-derived slug. A present but unparseable
// file is a tenant error that fails the run.
func (r Runner) loadTeams(ctx context.Context, owner, repo, ref string) (config.Teams, error) {
	data, err := r.GitHub.FileContent(ctx, owner, repo, TeamPath, ref)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s from %s/%s@%s: %w", TeamPath, owner, repo, ref, err)
	}
	teams, err := config.ParseTeams(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s from %s/%s@%s: %w", TeamPath, owner, repo, ref, err)
	}
	return teams, nil
}

// loadArchitecture reads the tenant's .github/talooner/architecture.yaml from
// the base branch and parses it into code_unit layer overrides. A missing file
// is an answer — the built-in per-language conventions decide unit kind and doc
// alone. A present but unparseable file is a tenant error that fails the run,
// the same shape as a broken modules.yaml.
func (r Runner) loadArchitecture(ctx context.Context, owner, repo, ref string) ([]config.ArchitectureRule, error) {
	data, err := r.GitHub.FileContent(ctx, owner, repo, ArchitecturePath, ref)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s from %s/%s@%s: %w", ArchitecturePath, owner, repo, ref, err)
	}
	arch, err := config.ParseArchitecture(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s from %s/%s@%s: %w", ArchitecturePath, owner, repo, ref, err)
	}
	return arch, nil
}

// resolveCodeUnits turns facts.PR's touched-unit records into the wire shape
// evaluate_pr sends as code_units (expert-review-system.md, Phase 2;
// talooner-plugin/docs/llm-review.md): each unit's governing doc, read from
// the base branch so a fork PR cannot rewrite what it is judged against.
//
// Gated on the repo having its own .github/talooner/architecture.yaml
// (arch non-empty): the built-in per-language layer table alone (Go's
// internal/<pkg>, Rails' app/models etc.) matches almost every changed file
// in almost every repo, onboarded for llm_review or not. Resolving docs and
// warning on every convention-derived unit with no doc would mean a doc-
// loading API call and a "doc not found" warning on every single run of every
// onboarded repo that has not written per-service docs — not a cost or a
// noise level a repo merely running Talooner for its other rules signed up
// for. Writing architecture.yaml, even a one-line rule, is the opt-in signal.
//
// A unit with no doc_ref (an architecture.yaml override that named none) is
// dropped silently — that is a declared "no doc to review against", not a
// problem (facts.md, "architecture.yaml: override or extend the built-in
// layers"). A unit whose doc_ref fails to load — missing on the base branch,
// or (github.FileContent's own cap) over 1 MiB, GitHub's own inline-content
// limit — is also dropped, but warns rather than failing the run: llm_review
// is additive, never a reason to withhold the rest of the verdict. Every doc
// is fetched at most once per run, keyed by doc_ref, since several units can
// share one doc, and a failure warns once per doc_ref, not once per unit.
//
// This never actually returns a non-nil error today — every FileContent
// failure degrades to a warning instead — but it keeps the same (result,
// warnings, error) shape as its caller's other loadX methods rather than
// being the one exception.
func (r Runner) resolveCodeUnits(ctx context.Context, owner, repo, baseRef string, units []facts.CodeUnit, arch []config.ArchitectureRule) ([]cluster.CodeUnit, []check.Warning, error) {
	if len(arch) == 0 {
		return nil, nil, nil
	}
	type doc struct {
		content []byte
		err     error
	}
	docs := make(map[string]doc, len(units))
	var warnings []check.Warning
	result := make([]cluster.CodeUnit, 0, len(units))

	for _, u := range units {
		if u.DocRef == "" {
			continue
		}
		d, cached := docs[u.DocRef]
		if !cached {
			content, err := r.GitHub.FileContent(ctx, owner, repo, u.DocRef, baseRef)
			d = doc{content: content, err: err}
			docs[u.DocRef] = d
			if err != nil {
				r.Log.Warn("code unit's doc could not be loaded, unit not reviewed",
					"repo", owner+"/"+repo, "unit", u.Path, "doc_ref", u.DocRef, "ref", baseRef, "err", err)
				warnings = append(warnings, check.Warning{
					Code:    "code_unit_doc_unavailable",
					Message: fmt.Sprintf("%s: doc %s not reviewed: %s", u.Path, u.DocRef, err),
				})
			}
		}
		if d.err != nil {
			continue
		}
		result = append(result, cluster.CodeUnit{
			Name:       u.Path,
			Important:  u.Important,
			DocURL:     u.DocRef,
			DocContent: string(d.content),
			Diff:       u.DiffSlice,
		})
	}
	return result, warnings, nil
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
// actions is already decoded by the caller — a verdict carrying a verb this
// build has never heard of fails the run before report is ever called, rather
// than after this has written a check run describing half of it.
//
// docWarnings are bot-side code_unit doc-loading problems (resolveCodeUnits) —
// folded in alongside the plugin's own warnings so both surface in the same
// check run and sticky comment, rather than only one of the two halves of the
// run.
//
// unitCount is len(codeUnits) as resolved by the caller — how many code_unit
// records were sent for llm_review, so the check run can say how many units
// were in scope. It cannot say how many drifted (check.Decision's doc comment
// has why).
func (r Runner) report(ctx context.Context, repo string, pr *github.PullRequest, resp *taloonerpb.EvaluatePrResponse, actions []action.Action, teams config.Teams, docWarnings []check.Warning, unitCount int) error {
	warnings := make([]check.Warning, 0, len(resp.GetWarnings())+len(docWarnings))
	warnings = append(warnings, docWarnings...)
	for _, w := range resp.GetWarnings() {
		r.Log.Warn("plugin warning", "code", w.GetCode(), "message", w.GetMessage())
		warnings = append(warnings, check.Warning{Code: w.GetCode(), Message: w.GetMessage()})
	}
	summary := resp.GetExplain().GetSummary()
	if summary != "" {
		r.Log.Info("decision", "repo", repo, "pr", r.Event.PR, "summary", summary)
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
	asg, err := assignment.New(r.GitHub, r.Event.Owner, r.Event.Repo, r.Event.PR, pr, actions, teams, r.Log)
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
	cr := check.Decision(actions, warnings, summary, unitCount)
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

// why answers `/why`: the plugin's persisted decision for the PR's current
// head sha (cluster.Client.ExplainPR), posted with GitHub.CreateComment
// rather than through sticky — a later `/why` at a later sha is a different
// question, not an edit to this one's answer (comment.Why).
func (r Runner) why(ctx context.Context, repo string, pr *github.PullRequest) error {
	resp, err := r.Cluster.ExplainPR(ctx, repo, r.Event.PR, pr.HeadSHA)
	if err != nil {
		if !errors.Is(err, cluster.ErrAction) {
			return fmt.Errorf("explain %s#%d: %w", repo, r.Event.PR, err)
		}
		// The plugin reached a clear answer: nothing was ever evaluated at this
		// sha. Worth telling the commander, not worth failing the run over.
		if _, cErr := r.GitHub.CreateComment(ctx, r.Event.Owner, r.Event.Repo, r.Event.PR,
			comment.WhyNotEvaluated(err.Error(), pr.HeadSHA)); cErr != nil {
			return fmt.Errorf("write why-unavailable comment for %s#%d: %w", repo, r.Event.PR, cErr)
		}
		return nil
	}

	if _, err := r.GitHub.CreateComment(ctx, r.Event.Owner, r.Event.Repo, r.Event.PR,
		comment.Why(resp.GetExplain(), pr.HeadSHA)); err != nil {
		return fmt.Errorf("write why comment for %s#%d: %w", repo, r.Event.PR, err)
	}
	return nil
}

// planReply answers the manual `/plan`: what the head-branch ruleset would
// decide right now, evaluated in cluster.ModePlan so nothing here is ever
// written — ModePlan's response carries the decision in a field EvaluatePR
// itself refuses to populate with anything a caller could execute. Facts still
// come from the base branch, the same fork-safety rule evaluate() follows, so
// a plan and a real run never see different facts for the same PR.
//
// Posted as a plain new comment, the same shape as why: this answers one
// specific ask at one specific sha, not an ongoing verdict to keep current in
// place like the sticky TopicPlan diff E2 posts automatically on fork PRs —
// that one compares base decision against head ruleset; this one has no base
// to compare against, it just answers "what would /review decide".
func (r Runner) planReply(ctx context.Context, repo string, pr *github.PullRequest) error {
	ev := r.Event

	cfg, err := r.loadConfig(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("load config for %s#%d: %w", repo, ev.PR, err)
	}
	codeowners, err := r.loadCodeowners(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("load codeowners for %s#%d: %w", repo, ev.PR, err)
	}
	modules, err := r.loadModules(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("load modules for %s#%d: %w", repo, ev.PR, err)
	}
	teams, err := r.loadTeams(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("load teams for %s#%d: %w", repo, ev.PR, err)
	}
	arch, err := r.loadArchitecture(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("load architecture for %s#%d: %w", repo, ev.PR, err)
	}

	ruleset, err := r.GitHub.FileContent(ctx, ev.Owner, ev.Repo, RulesetPath, pr.HeadSHA)
	if errors.Is(err, github.ErrNotFound) {
		if _, cErr := r.GitHub.CreateComment(ctx, ev.Owner, ev.Repo, ev.PR,
			comment.PlanNoRuleset(RulesetPath, pr.HeadSHA)); cErr != nil {
			return fmt.Errorf("write plan-no-ruleset comment for %s#%d: %w", repo, ev.PR, cErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load head ruleset for %s#%d: %w", repo, ev.PR, err)
	}

	set, units, err := facts.PR(ctx, r.GitHub, ev.Owner, ev.Repo, ev.PR, cfg.Checks, codeowners, modules, teams, arch)
	if err != nil {
		return err // already names the repo and PR, and is never partial
	}
	// Warnings from a bad doc are not surfaced here: /plan writes one informal
	// comment, not a check run, and resolveCodeUnits already logs them.
	codeUnits, _, err := r.resolveCodeUnits(ctx, ev.Owner, ev.Repo, pr.BaseRef, units, arch)
	if err != nil {
		return fmt.Errorf("load code unit docs for %s#%d: %w", repo, ev.PR, err)
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:      repo,
		PR:        ev.PR,
		HeadSHA:   pr.HeadSHA,
		Facts:     set,
		Ruleset:   string(ruleset),
		Mode:      cluster.ModePlan,
		CodeUnits: codeUnits,
	})
	if err != nil {
		if !errors.Is(err, cluster.ErrAction) {
			return fmt.Errorf("evaluate plan for %s#%d: %w", repo, ev.PR, err)
		}
		// The plugin ran and refused, most likely a head ruleset that will not
		// compile. Worth telling the commander, not worth failing the run over —
		// the same call why makes for a sha with no recorded decision.
		if _, cErr := r.GitHub.CreateComment(ctx, ev.Owner, ev.Repo, ev.PR,
			comment.PlanBroken(err.Error(), pr.HeadSHA)); cErr != nil {
			return fmt.Errorf("write plan-broken comment for %s#%d: %w", repo, ev.PR, cErr)
		}
		return nil
	}

	actions, err := action.FromProtos(resp.GetPlan())
	if err != nil {
		return fmt.Errorf("decode the plan for %s#%d: %w", repo, ev.PR, err)
	}

	if _, err := r.GitHub.CreateComment(ctx, ev.Owner, ev.Repo, ev.PR,
		comment.PlanNow(actions, pr.HeadSHA)); err != nil {
		return fmt.Errorf("write plan comment for %s#%d: %w", repo, ev.PR, err)
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

// configBroken writes the review comment for a .github/talooner/*.yaml file
// this run could not use — malformed YAML, an unreadable CODEOWNERS, a
// modules.yaml path that escapes the repository, or a field shaped like a
// credential. The underlying yaml.v3 error already names the file (each loader
// wraps it with the path) and the line, so comment.Broken needs nothing extra
// to satisfy the ticket's "name the file and line" requirement.
//
// Unlike rulesetBroken it does not write the check run itself: the cause is
// returned unwrapped, so Run's failOpen writes the neutral check the same way
// it does for any other run-time failure. Only the comment needs the earlier,
// more specific wording.
func (r Runner) configBroken(ctx context.Context, repo string, pr *github.PullRequest, cause error) error {
	if err := r.sticky(ctx, comment.TopicReview, comment.Broken(cause.Error(), pr.HeadSHA), false); err != nil {
		r.Log.Error("cannot write the review comment", "repo", repo, "pr", r.Event.PR, "err", err)
	}
	return cause
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
