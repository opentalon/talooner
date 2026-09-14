package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

const ActionValidateRuleset = "validate_ruleset"

func (c *Client) ValidateRuleset(ctx context.Context, src string) (*taloonerpb.ValidateRulesetResponse, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("validate_ruleset needs a ruleset")
	}
	var resp taloonerpb.ValidateRulesetResponse
	if err := c.Execute(ctx, ActionValidateRuleset, map[string]string{"ruleset": src}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
