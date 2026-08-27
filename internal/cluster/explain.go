package cluster

import (
	"context"
	"fmt"
	"strconv"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// ActionExplainPR reads back the persisted decision for a PR at a given sha.
const ActionExplainPR = "explain_pr"

// ExplainPR answers `/why`: the stored explanation for repo#pr's decision at
// headSHA, read-only. It never re-evaluates, so it still answers once the
// facts behind that decision have aged out of retention, and it fails with
// ErrAction when the sha was never evaluated at all rather than returning an
// empty explanation that would read like "no rules fired".
func (c *Client) ExplainPR(ctx context.Context, repo string, pr int, headSHA string) (*taloonerpb.ExplainPrResponse, error) {
	if repo == "" || pr <= 0 {
		return nil, fmt.Errorf("explain_pr needs owner/name and a positive pr number, got %q#%d", repo, pr)
	}
	if headSHA == "" {
		return nil, fmt.Errorf("explain_pr needs a head sha for %s#%d", repo, pr)
	}

	var resp taloonerpb.ExplainPrResponse
	args := map[string]string{"repo": repo, "pr": strconv.Itoa(pr), "head_sha": headSHA}
	if err := c.Execute(ctx, ActionExplainPR, args, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
