package run

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/opentalon/talooner/internal/action"
	"github.com/opentalon/talooner/internal/cluster"
	"github.com/opentalon/talooner/internal/facts"
)

// Plan evaluates a live PR's base-branch ruleset in plan mode and renders the
// actions that would fire to w — the CLI half of F4 (talooner rules plan
// --repo --pr). It reads the ruleset, config.yaml, CODEOWNERS, modules.yaml
// and teams.yaml the same way evaluate does, so a plan and the real run can
// never see different facts for the same PR.
//
// Two things make "zero writes" true rather than merely conventional:
// cluster.ModePlan, which the plugin answers through a response field a
// caller cannot mistake for something executable (EvaluatePR itself refuses
// a plan-mode response carrying actions); and action.Printing, the same
// Executor interface the real run's registry uses, with every verb mapped to
// a renderer instead of a GitHub call. Neither r.GitHub nor r.Cluster is ever
// asked to write anything here.
func (r Runner) Plan(ctx context.Context, owner, repo string, prNum int, w io.Writer) error {
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	fullRepo := owner + "/" + repo

	pr, err := r.GitHub.PullRequest(ctx, owner, repo, prNum)
	if err != nil {
		return fmt.Errorf("fetch %s#%d: %w", fullRepo, prNum, err)
	}
	if pr.BaseRef == "" {
		return fmt.Errorf("%s#%d came back with no base ref", fullRepo, prNum)
	}

	ruleset, err := r.GitHub.FileContent(ctx, owner, repo, RulesetPath, pr.BaseRef)
	if err != nil {
		return fmt.Errorf("load ruleset: %w", err)
	}
	cfg, err := r.loadConfig(ctx, owner, repo, pr.BaseRef)
	if err != nil {
		return err
	}
	codeowners, err := r.loadCodeowners(ctx, owner, repo, pr.BaseRef)
	if err != nil {
		return err
	}
	modules, err := r.loadModules(ctx, owner, repo, pr.BaseRef)
	if err != nil {
		return err
	}
	teams, err := r.loadTeams(ctx, owner, repo, pr.BaseRef)
	if err != nil {
		return err
	}
	arch, err := r.loadArchitecture(ctx, owner, repo, pr.BaseRef)
	if err != nil {
		return err
	}

	set, err := facts.PR(ctx, r.GitHub, owner, repo, prNum, cfg.Checks, codeowners, modules, teams, arch)
	if err != nil {
		return err
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:    fullRepo,
		PR:      prNum,
		HeadSHA: pr.HeadSHA,
		Facts:   set,
		Ruleset: string(ruleset),
		Mode:    cluster.ModePlan,
	})
	if err != nil {
		return fmt.Errorf("evaluate %s#%d: %w", fullRepo, prNum, err)
	}

	planned, err := action.FromProtos(resp.GetPlan())
	if err != nil {
		return fmt.Errorf("decode the plan for %s#%d: %w", fullRepo, prNum, err)
	}

	if err := action.Printing(w).Execute(ctx, planned); err != nil {
		return fmt.Errorf("render the plan for %s#%d: %w", fullRepo, prNum, err)
	}
	return nil
}
