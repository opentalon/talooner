package cluster

import (
	"context"
	"fmt"

	"github.com/opentalon/talooner-plugin/proto/taloonerpb"
)

type Identity struct {
	Tenant          string
	ProtocolVersion uint32
	Models          []string
	Features        []string
	Quota           Quota
}

type Quota struct {
	LLMCallsUsed  int64
	LLMCallsLimit int64
}

func (i Identity) HasFeature(name string) bool {
	for _, f := range i.Features {
		if f == name {
			return true
		}
	}
	return false
}

func (i Identity) HasModel(name string) bool {
	for _, m := range i.Models {
		if m == name {
			return true
		}
	}
	return false
}

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
