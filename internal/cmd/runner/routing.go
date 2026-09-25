// Package main — routing.go converts the CRD-facing
// v1alpha1.OpenRouterRouting (stamped onto AgentSession.status.effectiveSettings
// by pkg/platform/settings) into the neutral llm.OpenRouterRouting the runner's Loop
// carries through to the LLM request. Kept out of pkg/apis/v1alpha1 so that
// package stays free of a pkg/agent/llm import; kept out of pkg/agent/runner
// so Loop stays v1alpha1-free.
package main

import (
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// routingToLLM converts the resolved catalog routing preferences to the
// neutral llm.OpenRouterRouting shape. nil in ⇒ nil out. The two struct
// shapes are field-for-field identical (see the "keep in sync" note on
// v1alpha1.OpenRouterRouting); every slice/pointer field is freshly
// allocated so the returned value never aliases the (controller-owned)
// status object it was read from.
func routingToLLM(r *spiceboxv1alpha1.OpenRouterRouting) *llm.OpenRouterRouting {
	if r == nil {
		return nil
	}
	out := &llm.OpenRouterRouting{
		Sort:           r.Sort,
		DataCollection: r.DataCollection,
	}
	if r.Models != nil {
		out.Models = append([]string{}, r.Models...)
	}
	if r.Order != nil {
		out.Order = append([]string{}, r.Order...)
	}
	if r.Only != nil {
		out.Only = append([]string{}, r.Only...)
	}
	if r.Ignore != nil {
		out.Ignore = append([]string{}, r.Ignore...)
	}
	if r.AllowFallbacks != nil {
		v := *r.AllowFallbacks
		out.AllowFallbacks = &v
	}
	if r.RequireParameters != nil {
		v := *r.RequireParameters
		out.RequireParameters = &v
	}
	if r.MaxPrice != nil {
		out.MaxPrice = &llm.OpenRouterMaxPrice{
			Prompt:     r.MaxPrice.Prompt,
			Completion: r.MaxPrice.Completion,
		}
	}
	if r.AllowedModels != nil {
		out.AllowedModels = append([]string{}, r.AllowedModels...)
	}
	if r.CostQualityTradeoff != nil {
		v := *r.CostQualityTradeoff
		out.CostQualityTradeoff = &v
	}
	return out
}
