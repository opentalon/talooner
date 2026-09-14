package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

const ActionGenerateRuleset = "generate_ruleset"

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
