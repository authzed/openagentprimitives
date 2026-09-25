package cost

import (
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/x/llmpricing"
)

// orderSessionCost places the cost report after SessionCleanup (10) at the
// SessionEnd point. Both are Allow-only; order is cosmetic, but later is tidy
// (cleanup audit first, then the cost summary).
const orderSessionCost = 20

func init() {
	runner.RegisterHook(runner.HookFactory{
		Name:  "session_cost",
		Order: orderSessionCost,
		Build: func(l *runner.Loop) []pipeline.Hook {
			if !l.ReportSessionCost || l.Provider == nil {
				return nil
			}
			return []pipeline.Hook{New(Deps{
				Pricing: catalogFirstPricing(l),
				Stamp:   l.StampEstimatedCost,
			})}
		},
	})
}

// catalogFirstPricing prefers the resolved catalog price (operator-authoritative,
// the same source the admin dashboard uses) over the provider's built-in table,
// so a session's estimate matches the dashboard. Cache buckets derive from the
// catalog input price via the standard ratios. It falls back to the provider's
// Pricing when the catalog has no price — AND for any model other than l.Model.
//
// That second condition is the point: the catalog price is a fixed rate the admin
// set for ONE entry, the session's CONFIGURED model, not for whatever model this
// func is asked about. Per-model cost buckets can price a DIFFERENT actually-served
// model in the same session (OpenRouter auto-routing away from "openrouter/auto"),
// and such a bucket must fall through to the provider's table rather than silently
// inherit the configured model's rate.
func catalogFirstPricing(l *runner.Loop) func(model string) (llm.ModelPricing, bool) {
	return func(model string) (llm.ModelPricing, bool) {
		if model == l.Model && (l.ModelInputPerMTok > 0 || l.ModelOutputPerMTok > 0) {
			return llmpricing.WithCacheRatios(l.ModelInputPerMTok, l.ModelOutputPerMTok), true
		}
		return l.Provider.Pricing(model)
	}
}
