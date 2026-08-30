package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// The actions this package calls beyond the handshake. They are the plugin's
// declared action names (talooner-plugin/internal/service/actions.go), not rpcs.
const (
	ActionEvaluatePR      = "evaluate_pr"
	ActionIsSubscribed    = "is_subscribed"
	ActionSetSubscription = "set_subscription"
)

// Mode selects what evaluate_pr returns. Plan mode is how a ruleset from an
// untrusted head branch is evaluated: the decision comes back in the response's
// separate plan field, so nothing a plan produces can be handed to an executor
// by accident.
type Mode string

const (
	ModeExecute Mode = "execute"
	ModePlan    Mode = "plan"
)

// CodeUnit is one touched, documented unit of code offered for llm_review,
// wire-compatible with the plugin's code_units arg
// (talooner-plugin/internal/service/units.go) — field names and JSON tags
// must match its codeUnit struct exactly, since this is decoded there, not
// generated from a shared proto.
type CodeUnit struct {
	Name          string `json:"name"`
	Important     bool   `json:"important"`
	DocURL        string `json:"doc_url"`
	DocContent    string `json:"doc_content"`
	Diff          string `json:"diff"`
	DiffTruncated bool   `json:"diff_truncated"`
}

// EvaluateRequest is one evaluation. Repo is owner/name — the plugin scopes
// facts by "{repo}#{pr}" and takes the two halves joined.
type EvaluateRequest struct {
	Repo    string
	PR      int
	HeadSHA string
	// Facts is the extracted fact set, sent as the facts JSON arg. Values must
	// be bool, int, string or []string; the plugin rejects null, nested objects
	// and mixed arrays outright.
	Facts   map[string]any
	Ruleset string
	Mode    Mode
	// Force bypasses the llm_review fact cache for this evaluation. v1 parses
	// --force and rejects it, so this is false until llm_review lands.
	Force bool
	// CodeUnits are the touched, documented units this PR changed (expert-
	// review-system.md, Phase 2); each becomes a code_unit record cluster-side.
	// Nil means no code_units arg at all, not an empty JSON list — a ruleset
	// with no llm_review rules pays nothing extra either way, but a PR touching
	// nothing under a known layer is the common case and should not send an
	// arg for it.
	CodeUnits []CodeUnit
}

// EvaluatePR compiles a ruleset against a PR's facts and returns the decision.
//
// The response is the generated type rather than a local struct: it is the
// contract, and a hand-rolled copy would be one more thing to keep in step with
// a cluster that upgrades on its own schedule.
func (c *Client) EvaluatePR(ctx context.Context, req EvaluateRequest) (*taloonerpb.EvaluatePrResponse, error) {
	if req.Repo == "" || req.PR <= 0 {
		return nil, fmt.Errorf("evaluate_pr needs owner/name and a positive pr number, got %q#%d", req.Repo, req.PR)
	}
	if req.HeadSHA == "" {
		return nil, fmt.Errorf("evaluate_pr needs a head sha for %s#%d", req.Repo, req.PR)
	}
	// An empty fact set is not the same as an empty JSON object here: the plugin
	// re-derives from what arrives and drops every bot fact absent from the
	// request, so sending nothing retracts everything it knew.
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
	// Plan mode populating actions would mean a dry run came back executable.
	// The plugin keeps the fields distinct; this is the second check, because
	// the whole point is that a plan can never be executed by accident.
	if mode == ModePlan && len(resp.GetActions()) > 0 {
		return nil, fmt.Errorf("%w: %s returned %d executable actions in plan mode",
			ErrAction, ActionEvaluatePR, len(resp.GetActions()))
	}
	return &resp, nil
}

// IsSubscribed reports whether Talooner is watching a PR. A PR nobody has ever
// invoked it on is simply not subscribed — false, no error — which is what makes
// the cheap path a skipped job rather than a red check.
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

// SetSubscription subscribes or unsubscribes a PR and returns the state the
// plugin ended up in. Setting the same state twice is a no-op cluster-side, so a
// re-run of /review does not need to check first.
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

// scopeArgs is the {repo, pr} pair every scoped action takes.
func scopeArgs(repo string, pr int) (map[string]string, error) {
	if repo == "" || pr <= 0 {
		return nil, fmt.Errorf("scope needs owner/name and a positive pr number, got %q#%d", repo, pr)
	}
	return map[string]string{"repo": repo, "pr": strconv.Itoa(pr)}, nil
}
