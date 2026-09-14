package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

const ActionRunRulesetTest = "run_ruleset_test"

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
