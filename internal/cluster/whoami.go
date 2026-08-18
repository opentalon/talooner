package cluster

import (
	"context"
	"fmt"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

// Identity is what the cluster reported about itself and the tenant at the
// handshake. It is the capability answer, not just an identity check: a caller
// learns from it whether llm_review is available before loading a ruleset that
// depends on it.
type Identity struct {
	Tenant          string
	ProtocolVersion uint32
	Models          []string
	Features        []string
	Quota           Quota
}

// Quota is the tenant's LLM budget for the current window. A limit of zero
// means the cluster reported none, not that the tenant has none — quota
// exhaustion is enforced cluster-side and surfaces as an llm_review fact, so
// this is for display, never for a local decision.
type Quota struct {
	LLMCallsUsed  int64
	LLMCallsLimit int64
}

// HasFeature reports whether the cluster advertised a named feature.
func (i Identity) HasFeature(name string) bool {
	for _, f := range i.Features {
		if f == name {
			return true
		}
	}
	return false
}

// HasModel reports whether the cluster advertised a named model.
func (i Identity) HasModel(name string) bool {
	for _, m := range i.Models {
		if m == name {
			return true
		}
	}
	return false
}

// whoami performs the handshake. Every failure it can produce is distinct and
// terminal, because each one is a different thing for a tenant to go fix:
//
//   - the call itself failing, or the plugin refusing the key, is ErrHandshake
//     carrying the plugin's message — which for a below-floor caller names the
//     floor to upgrade past;
//   - a cluster older than this action's floor is ErrProtocolSkew, the one
//     direction of skew the plugin cannot detect;
//   - a response with no tenant is a handshake that "succeeded" against
//     something that is not this plugin, which must not read as success.
func (c *Client) whoami(ctx context.Context) (Identity, error) {
	var resp taloonerpb.WhoamiResponse
	if err := c.Execute(ctx, ActionWhoami, nil, &resp); err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrHandshake, err)
	}
	if resp.GetTenant() == "" {
		return Identity{}, fmt.Errorf("%w: whoami returned no tenant", ErrHandshake)
	}
	if v := resp.GetProtocolVersion(); v < ProtocolFloor {
		return Identity{}, fmt.Errorf("%w: cluster speaks protocol %d, this action requires at least %d; upgrade talooner-plugin",
			ErrProtocolSkew, v, ProtocolFloor)
	}
	return Identity{
		Tenant:          resp.GetTenant(),
		ProtocolVersion: resp.GetProtocolVersion(),
		Models:          resp.GetModels(),
		Features:        resp.GetFeatures(),
		Quota: Quota{
			LLMCallsUsed:  resp.GetQuota().GetLlmCallsUsed(),
			LLMCallsLimit: resp.GetQuota().GetLlmCallsLimit(),
		},
	}, nil
}
