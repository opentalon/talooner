package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// ActionRunRulesetTest compiles a ruleset with its paired .tln.test source and
// runs the assertions, returning pass/fail per test block.
const ActionRunRulesetTest = "run_ruleset_test"

// RunRulesetTest runs testSrc's assertions against src, the same way `tln test`
// would locally — round-tripped to the cluster for the same reason
// ValidateRuleset is: a tenant's CI and the plugin can never disagree about
// whether a rule passes its own tests, because it is the same code path.
func (c *Client) RunRulesetTest(ctx context.Context, src, testSrc string) (*taloonerpb.RunRulesetTestResponse, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("run_ruleset_test needs a ruleset")
	}
	if strings.TrimSpace(testSrc) == "" {
		return nil, fmt.Errorf("run_ruleset_test needs a test source")
	}
	var resp taloonerpb.RunRulesetTestResponse
	args := map[string]string{"ruleset": src, "test_source": testSrc}
	if err := c.Execute(ctx, ActionRunRulesetTest, args, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
