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

	set, units, err := facts.PR(ctx, r.GitHub, owner, repo, prNum, cfg.Checks, codeowners, modules, teams, arch)
	if err != nil {
		return err
	}
	codeUnits, _, err := r.resolveCodeUnits(ctx, owner, repo, pr.BaseRef, units, arch)
	if err != nil {
		return err
	}

	resp, err := r.Cluster.EvaluatePR(ctx, cluster.EvaluateRequest{
		Repo:      fullRepo,
		PR:        prNum,
		HeadSHA:   pr.HeadSHA,
		Facts:     set,
		Ruleset:   string(ruleset),
		Mode:      cluster.ModePlan,
		CodeUnits: codeUnits,
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
