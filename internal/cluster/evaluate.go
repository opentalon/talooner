package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

const (
	ActionEvaluatePR      = "evaluate_pr"
	ActionIsSubscribed    = "is_subscribed"
	ActionSetSubscription = "set_subscription"
)

type Mode string

const (
	ModeExecute Mode = "execute"
	ModePlan    Mode = "plan"
)

type CodeUnit struct {
	Name          string `json:"name"`
	Important     bool   `json:"important"`
	DocURL        string `json:"doc_url"`
	DocContent    string `json:"doc_content"`
	Diff          string `json:"diff"`
	DiffTruncated bool   `json:"diff_truncated"`
	TestDiff      string `json:"test_diff"`
}

type EvaluateRequest struct {
	Repo      string
	PR        int
	HeadSHA   string
	Facts     map[string]any
	Ruleset   string
	Mode      Mode
	Force     bool
	CodeUnits []CodeUnit
}

func (c *Client) EvaluatePR(ctx context.Context, req EvaluateRequest) (*taloonerpb.EvaluatePrResponse, error) {
	if req.Repo == "" || req.PR <= 0 {
		return nil, fmt.Errorf("evaluate_pr needs owner/name and a positive pr number, got %q#%d", req.Repo, req.PR)
	}
	if req.HeadSHA == "" {
		return nil, fmt.Errorf("evaluate_pr needs a head sha for %s#%d", req.Repo, req.PR)
	}
	if len(req.Facts) == 0 {
		return nil, fmt.Errorf("evaluate_pr for %s#%d carries no facts", req.Repo, req.PR)
	}

	factsJSON, err := json.Marshal(req.Facts)
	if err != nil {
		return nil, fmt.Errorf("encode facts for %s#%d: %w", req.Repo, req.PR, err)
	}

	mode := req.Mode
	if mode == "" {
		mode = ModeExecute
	}
	args := map[string]string{
		"repo":     req.Repo,
		"pr":       strconv.Itoa(req.PR),
		"head_sha": req.HeadSHA,
		"facts":    string(factsJSON),
		"ruleset":  req.Ruleset,
		"mode":     string(mode),
		"force":    strconv.FormatBool(req.Force),
	}
	if len(req.CodeUnits) > 0 {
		codeUnitsJSON, err := json.Marshal(req.CodeUnits)
		if err != nil {
			return nil, fmt.Errorf("encode code_units for %s#%d: %w", req.Repo, req.PR, err)
		}
		args["code_units"] = string(codeUnitsJSON)
	}

	var resp taloonerpb.EvaluatePrResponse
	if err := c.Execute(ctx, ActionEvaluatePR, args, &resp); err != nil {
		return nil, err
	}
	if mode == ModePlan && len(resp.GetActions()) > 0 {
		return nil, fmt.Errorf("%w: %s returned %d executable actions in plan mode",
			ErrAction, ActionEvaluatePR, len(resp.GetActions()))
	}
	return &resp, nil
}

func (c *Client) IsSubscribed(ctx context.Context, repo string, pr int) (bool, error) {
	args, err := scopeArgs(repo, pr)
	if err != nil {
		return false, err
	}
	var resp taloonerpb.IsSubscribedResponse
	if err := c.Execute(ctx, ActionIsSubscribed, args, &resp); err != nil {
		return false, err
	}
	return resp.GetSubscribed(), nil
}

func (c *Client) SetSubscription(ctx context.Context, repo string, pr int, state bool) (bool, error) {
	args, err := scopeArgs(repo, pr)
	if err != nil {
		return false, err
	}
	args["state"] = strconv.FormatBool(state)

	var resp taloonerpb.SetSubscriptionResponse
	if err := c.Execute(ctx, ActionSetSubscription, args, &resp); err != nil {
		return false, err
	}
	return resp.GetSubscribed(), nil
}

func scopeArgs(repo string, pr int) (map[string]string, error) {
	if repo == "" || pr <= 0 {
		return nil, fmt.Errorf("scope needs owner/name and a positive pr number, got %q#%d", repo, pr)
	}
	return map[string]string{"repo": repo, "pr": strconv.Itoa(pr)}, nil
}
