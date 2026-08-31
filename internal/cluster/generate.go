package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// ActionGenerateRuleset scaffolds a rules.tln + rules.tln.test pair for a repo.
const ActionGenerateRuleset = "generate_ruleset"

// GenerateRuleset asks the cluster to scaffold a ruleset from a repo summary
// — round-tripped the same way ValidateRuleset and RunRulesetTest are, so a
// tenant's CI and the plugin can never disagree about what the plugin
// actually returned. On the LLM path the plugin has already self-verified
// the pair compiles and passes; on Source == "fallback" both Ruleset and
// RulesetTest come back empty and Note explains why — the caller supplies
// its own starter in that case (`talooner onboard` does).
func (c *Client) GenerateRuleset(ctx context.Context, repoSummary string) (*taloonerpb.GenerateRulesetResponse, error) {
	if strings.TrimSpace(repoSummary) == "" {
		return nil, fmt.Errorf("generate_ruleset needs a repo summary")
	}
	var resp taloonerpb.GenerateRulesetResponse
	args := map[string]string{"repo_summary": repoSummary}
	if err := c.Execute(ctx, ActionGenerateRuleset, args, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
