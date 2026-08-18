package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// ActionValidateRuleset compiles a ruleset and returns positioned diagnostics.
const ActionValidateRuleset = "validate_ruleset"

// ValidateRuleset compiles src without evaluating anything and returns what the
// compiler found.
//
// A run reaches for this after evaluate_pr refused: evaluate_pr reports a
// compile failure as a plain error string with no position in it, and a check
// run annotation has to name a line. So the failing path pays one extra call to
// learn where the ruleset broke, rather than the happy path paying for a
// validation it already got by compiling.
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
