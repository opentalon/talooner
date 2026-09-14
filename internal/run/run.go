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

const RulesetPath = ".github/talooner/rules.tln"

const ConfigPath = ".github/talooner/config.yaml"

const ModulePath = ".github/talooner/modules.yaml"

const TeamPath = ".github/talooner/teams.yaml"

const ArchitecturePath = ".github/talooner/architecture.yaml"

type Runner struct {
	Event   *event.Event
	GitHub  *github.Client
	Cluster *cluster.Client
	Handle  string
	Log     *slog.Logger
}

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
	repo := ev.Owner + "/" + ev.Repo

	if ev.Trigger == event.TriggerIssueComment {
		cmd, err := r.gate(ctx)
		if err != nil || cmd == nil {
			return err
		}
		switch cmd.Verb {
		case command.VerbReview:
			if _, err := r.Cluster.SetSubscription(ctx, repo, ev.PR, true); err != nil {
				return fmt.Errorf("subscribe %s#%d: %w", repo, ev.PR, err)
			}
			if err := r.sticky(ctx, comment.TopicReview, comment.Acknowledge(), false); err != nil {
				r.Log.Warn("cannot write the acknowledge comment", "repo", repo, "pr", ev.PR, "err", err)
			}
		case command.VerbStop:
			if _, err := r.GitHub.CreateComment(ctx, ev.Owner, ev.Repo, ev.PR, comment.Stopped()); err != nil {
				return fmt.Errorf("write stopped comment for %s#%d: %w", repo, ev.PR, err)
			}
			if _, err := r.Cluster.SetSubscription(ctx, repo, ev.PR, false); err != nil {
				return fmt.Errorf("unsubscribe %s#%d: %w", repo, ev.PR, err)
			}
			r.Log.Info("unsubscribed", "repo", repo, "pr", ev.PR, "actor", ev.Actor)
			return nil
		case command.VerbWhy:
			pr, err := r.GitHub.PullRequest(ctx, ev.Owner, ev.Repo, ev.PR)
			if err != nil {
				return fmt.Errorf("fetch %s#%d: %w", repo, ev.PR, err)
			}
			return r.why(ctx, repo, pr)
		case command.VerbPlan:
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

	pr, err := r.GitHub.PullRequest(ctx, ev.Owner, ev.Repo, ev.PR)
	if err != nil {
		return fmt.Errorf("fetch %s#%d: %w", repo, ev.PR, err)
	}
	if ev.HeadSHA != "" && ev.HeadSHA != pr.HeadSHA {
		r.Log.Info("head sha moved since the event, leaving it to the newer run",
			"repo", repo, "pr", ev.PR, "event_sha", ev.HeadSHA, "current_sha", pr.HeadSHA)
		return nil
	}
	if pr.BaseRef == "" {
		return fmt.Errorf("%s#%d came back with no base ref", repo, ev.PR)
	}

	if err := r.evaluate(ctx, repo, pr); err != nil {
		return r.failOpen(ctx, repo, pr, err)
	}
	return nil
}

func (r Runner) evaluate(ctx context.Context, repo string, pr *github.PullRequest) error {
	ev := r.Event

	ruleset, err := r.GitHub.FileContent(ctx, ev.Owner, ev.Repo, RulesetPath, pr.BaseRef)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
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
		return r.configBroken(ctx, repo, pr, err)
	}

	codeowners, err := r.loadCodeowners(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}

	modules, err := r.loadModules(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}
	teams, err := r.loadTeams(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}
	arch, err := r.loadArchitecture(ctx, ev.Owner, ev.Repo, pr.BaseRef)
	if err != nil {
		return r.configBroken(ctx, repo, pr, err)
	}

	set, units, err := facts.PR(ctx, r.GitHub, ev.Owner, ev.Repo, ev.PR, cfg.Checks, codeowners, modules, teams, arch)
	if err != nil {
		return err
	}
	codeUnits, docWarnings, err := r.resolveCodeUnits(ctx, ev.Owner, ev.Repo, pr.BaseRef, units, arch)
	if err != nil {
		return fmt.Errorf("load code unit docs for %s#%d: %w", repo, ev.PR, err)
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:      repo,
		PR:        ev.PR,
		HeadSHA:   pr.HeadSHA,
		Facts:     set,
		Ruleset:   string(ruleset),
		Mode:      cluster.ModeExecute,
		CodeUnits: codeUnits,
	})
	if err != nil {
		evalErr := fmt.Errorf("evaluate %s#%d: %w", repo, ev.PR, err)
		if !errors.Is(err, cluster.ErrAction) {
			return evalErr
		}
		return r.rulesetBroken(ctx, repo, pr, string(ruleset), evalErr)
	}

	actions, err := action.FromProtos(resp.GetActions())
	if err != nil {
		return fmt.Errorf("decode the decision for %s#%d: %w", repo, ev.PR, err)
	}

	if pr.IsFork {
		if err := r.plan(ctx, repo, pr, set, codeUnits, actions); err != nil {
			r.Log.Warn("fork plan comparison did not complete", "repo", repo, "pr", ev.PR, "err", err)
		}
	}

	return r.report(ctx, repo, pr, resp, actions, teams, docWarnings, len(codeUnits))
}

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

var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

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
			TestDiff:   u.TestDiffSlice,
		})
	}
	return result, warnings, nil
}

func (r Runner) gate(ctx context.Context) (*command.Command, error) {
	ev := r.Event
	cmd, parseErr := command.Parse(r.Handle, ev.CommentBody)
	if errors.Is(parseErr, command.ErrNoCommand) {
		return nil, nil
	}

	if err := command.Authorize(ctx, r.GitHub, ev.Owner, ev.Repo, ev.Actor); err != nil {
		if errors.Is(err, command.ErrNotAuthorized) {
			r.Log.Info("ignoring command from an account without write access",
				"repo", ev.Owner+"/"+ev.Repo, "pr", ev.PR, "actor", ev.Actor)
			return nil, nil
		}
		return nil, err
	}

	if parseErr != nil {
		r.Log.Warn("command not understood", "actor", ev.Actor, "err", parseErr)
		if err := r.sticky(ctx, comment.TopicUsage, comment.Usage(command.Usage(r.Handle)), false); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return cmd, nil
}

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

	if err := r.reviewComment(ctx, repo, pr, actions, warnings, summary); err != nil {
		return err
	}

	if err := registry.Execute(ctx, actions); err != nil {
		return fmt.Errorf("carry out the decision for %s#%d: %w", repo, r.Event.PR, err)
	}
	if err := rev.Sync(ctx); err != nil {
		return err
	}
	if err := asg.Sync(ctx); err != nil {
		return fmt.Errorf("sync the assignments for %s#%d: %w", repo, r.Event.PR, err)
	}

	cr := check.Decision(actions, warnings, summary, unitCount)
	cr.HeadSHA = pr.HeadSHA
	if _, err := r.GitHub.UpsertCheckRun(ctx, r.Event.Owner, r.Event.Repo, cr); err != nil {
		return fmt.Errorf("write the check run for %s#%d: %w", repo, r.Event.PR, err)
	}
	r.Log.Info("check run written", "repo", repo, "pr", r.Event.PR,
		"sha", pr.HeadSHA, "conclusion", cr.Conclusion, "actions", len(actions))
	return nil
}

func (r Runner) reviewComment(ctx context.Context, repo string, pr *github.PullRequest,
	actions []action.Action, warnings []check.Warning, summary string,
) error {
	body, editOnly := comment.Review(actions, warnings, summary, pr.HeadSHA), false
	switch {
	case comment.Empty(actions, warnings) && len(actions) == 0:
		body, editOnly = comment.NoRulesFired(pr.HeadSHA), true
	case comment.Empty(actions, warnings):
		body, editOnly = comment.Resolved(pr.HeadSHA), true
	}
	if err := r.sticky(ctx, comment.TopicReview, body, editOnly); err != nil {
		return fmt.Errorf("write the review comment for %s#%d: %w", repo, r.Event.PR, err)
	}
	return nil
}

func (r Runner) why(ctx context.Context, repo string, pr *github.PullRequest) error {
	resp, err := r.Cluster.ExplainPR(ctx, repo, r.Event.PR, pr.HeadSHA)
	if err != nil {
		if !errors.Is(err, cluster.ErrAction) {
			return fmt.Errorf("explain %s#%d: %w", repo, r.Event.PR, err)
		}
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
		return err
	}
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
		return nil
	}
	r.Log.Info("sticky comment written",
		"repo", ev.Owner+"/"+ev.Repo, "pr", ev.PR, "topic", topic, "id", id)
	return nil
}

func (r Runner) rulesetBroken(ctx context.Context, repo string, pr *github.PullRequest, ruleset string, cause error) error {
	var diags []check.Diagnostic
	resp, err := r.Cluster.ValidateRuleset(ctx, ruleset)
	if err != nil {
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

	if err := r.sticky(ctx, comment.TopicReview, comment.Broken(cause.Error(), pr.HeadSHA), false); err != nil {
		r.Log.Error("cannot write the review comment", "repo", repo, "pr", r.Event.PR, "err", err)
	}
	return reported{cause}
}

func (r Runner) configBroken(ctx context.Context, repo string, pr *github.PullRequest, cause error) error {
	if err := r.sticky(ctx, comment.TopicReview, comment.Broken(cause.Error(), pr.HeadSHA), false); err != nil {
		r.Log.Error("cannot write the review comment", "repo", repo, "pr", r.Event.PR, "err", err)
	}
	return cause
}

func (r Runner) failOpen(ctx context.Context, repo string, pr *github.PullRequest, cause error) error {
	var already reported
	if errors.As(cause, &already) {
		return already.err
	}

	cr := check.Broken(cause.Error(), nil)
	cr.HeadSHA = pr.HeadSHA
	if _, err := r.GitHub.UpsertCheckRun(ctx, r.Event.Owner, r.Event.Repo, cr); err != nil {
		r.Log.Error("cannot write the neutral check run", "repo", repo, "pr", r.Event.PR, "err", err)
	}
	return cause
}

type reported struct{ err error }

func (r reported) Error() string { return r.err.Error() }
func (r reported) Unwrap() error { return r.err }

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

	cl, err := cluster.DialFromEnv(ctx, cluster.WithLogger(log))
	if err != nil {
		log.Error("cannot reach the cluster", "err", err)
		return 1
	}
	defer cl.Close() //nolint:errcheck

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
