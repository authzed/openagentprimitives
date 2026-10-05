// Package cost is the first post-session reporter: an end-of-session LLM cost
// estimate. It is an ordinary pipeline.Hook on the SessionEnd point, registered
// through the runner's hook-factory registry (see register.go). "post session
// hook" is the named home; future reporters (usage export, webhooks) join here.
package cost

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Deps are the cost reporter's construction dependencies. Usage/model/reason are
// NOT here — the hook reads those from pipeline.Input.End at Eval time.
type Deps struct {
	// Pricing returns per-model rates; ok=false ⇒ unknown model (no fabrication).
	Pricing func(model string) (llm.ModelPricing, bool)
	// Stamp persists the estimate to status.estimatedCost (a Decision cannot
	// write a CRD field, so the hook mutates via this injected handle).
	Stamp func(ctx context.Context, c v1.EstimatedSessionCost) error
	// Now supplies AsOf; nil ⇒ metav1.Now.
	Now func() metav1.Time
	// Quiet records accounting without sending a session-cost notice.
	Quiet bool
}

// Reporter is the SessionEnd cost-estimate hook.
type Reporter struct{ d Deps }

// New constructs a Reporter.
func New(d Deps) *Reporter {
	if d.Now == nil {
		d.Now = func() metav1.Time { return metav1.Now() }
	}
	return &Reporter{d: d}
}

func (r *Reporter) Name() string             { return "session_cost" }
func (r *Reporter) Points() []pipeline.Point { return []pipeline.Point{pipeline.SessionEnd} }

func (r *Reporter) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.End == nil {
		return pipeline.Decision{Verdict: pipeline.Allow}
	}
	e := in.End
	price, known := llm.ModelPricing{}, false
	if r.d.Pricing != nil {
		price, known = r.d.Pricing(e.Model)
	}

	// e.ByModel is empty on any runner build that predates per-model
	// accumulation (or a session served by exactly one model with no
	// breakdown wired) — that path is EXACTLY today's behavior, byte for
	// byte, so existing sessions/tests are unaffected. When present, the
	// session total and PricingKnown are DERIVED from the buckets (a sum
	// and an AND) rather than computed a second time from e.Model, so the
	// two can never disagree.
	var micro int64
	var buckets []v1.ModelCostBucket
	currency := price.Currency
	if len(e.ByModel) > 0 {
		var bucketCurrency string
		buckets, micro, known, bucketCurrency = r.buildCostBuckets(e.ByModel)
		if currency == "" {
			currency = bucketCurrency
		}
	} else if known {
		micro = microUSD(e, price)
	}

	// Interactive toolkit cost (e.g. an inner `claude` sub-run's own billed
	// spend) is provider-reported, so it is always "known" and is ADDED to the
	// model total. When a served model is unpriced (known=false, micro=0), the
	// grand total is still the priced tool component — a lower bound, per the
	// EstimatedSessionCost.AmountMicroUSD contract.
	toolBuckets, toolTotal := buildToolBuckets(e.ByTool)

	cost := v1.EstimatedSessionCost{
		AmountMicroUSD: micro + toolTotal,
		Currency:       currency,
		Model:          e.Model,
		PricingKnown:   known,
		AsOf:           r.d.Now(),
		ByModel:        buckets,
		ByTool:         toolBuckets,
	}
	if (known || toolTotal > 0) && cost.Currency == "" {
		cost.Currency = "USD"
	}
	// Stamp on EVERY path (cheap, gives admin UI a live running spend).
	if r.d.Stamp != nil {
		if err := r.d.Stamp(ctx, cost); err != nil {
			// no-silent-errors: log directly with structured context. Best-effort —
			// a failed stamp must not block teardown, and it must NOT suppress the
			// closing message (which does not depend on the stamp). We log here
			// rather than via a pipeline AuditRecord because the runner's audit
			// host has no case for a cost kind and would drop the err/model detail.
			slog.Default().Info("session_cost: stamp estimatedCost failed",
				"session", in.Session.String(), "model", e.Model, "err", err.Error())
		}
	}

	// Message ONLY on true completion/failure (never idle/await-user).
	if r.d.Quiet || (e.Reason != "completed" && e.Reason != "failed") {
		return pipeline.Decision{Verdict: pipeline.Allow}
	}
	return pipeline.Decision{
		Verdict: pipeline.Allow,
		Notices: []pipeline.Notice{{Notice: costNotice(e, cost, known), ToRequester: false}},
	}
}

// microUSD prices the session's cumulative token counts at p. It is a thin
// wrapper over tokensCostMicroUSD so the session-total path and the per-model
// bucket path below cannot drift into two implementations of one formula.
func microUSD(e *pipeline.SessionEndInfo, p llm.ModelPricing) int64 {
	return tokensCostMicroUSD(e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens, p)
}

// tokensCostMicroUSD = Σ tokens_bucket × ratePerMTok_bucket. (tokens ×
// USD-per-1e6-tokens == micro-USD, so the 1e6 cancels — no division, no
// float drift in the unit.) The one place this rate math is computed —
// microUSD (session total) and buildCostBuckets (per-model) both funnel
// through it.
func tokensCostMicroUSD(inputTok, outputTok, cacheCreationTok, cacheReadTok int64, p llm.ModelPricing) int64 {
	sum := float64(inputTok)*p.InputPerMTok +
		float64(outputTok)*p.OutputPerMTok +
		float64(cacheCreationTok)*p.CacheCreationPerMTok +
		float64(cacheReadTok)*p.CacheReadPerMTok
	return int64(math.Round(sum))
}

// buildCostBuckets prices each per-served-model usage bucket: the provider's
// reported cost when the runner captured one, else tokensCostMicroUSD, looked up
// by the bucket's BARE model id (m.Model, not m.Display) so an OpenRouter
// display id still resolves against the provider's own pricing table.
//
// Returns the priced buckets in caller order, their summed AmountMicroUSD (the
// session total when a breakdown exists), whether EVERY bucket priced, and the
// first priced bucket's currency — a fallback for the session Currency when the
// session-level Pricing(e.Model) came up empty, as under "openrouter/auto".
func (r *Reporter) buildCostBuckets(usages []pipeline.ModelUsage) (buckets []v1.ModelCostBucket, total int64, allKnown bool, currency string) {
	buckets = make([]v1.ModelCostBucket, len(usages))
	allKnown = true
	for i, m := range usages {
		b := v1.ModelCostBucket{Model: m.Display, InputTokens: m.InputTokens, OutputTokens: m.OutputTokens}
		switch {
		case m.CostReported:
			b.AmountMicroUSD = m.ReportedCostMicroUSD
			b.PricingKnown = true
		case r.d.Pricing != nil:
			if p, ok := r.d.Pricing(m.Model); ok {
				b.AmountMicroUSD = tokensCostMicroUSD(m.InputTokens, m.OutputTokens, m.CacheCreationTokens, m.CacheReadTokens, p)
				b.PricingKnown = true
				if currency == "" {
					currency = p.Currency
				}
			}
		}
		if !b.PricingKnown {
			allKnown = false
		}
		total += b.AmountMicroUSD
		buckets[i] = b
	}
	return buckets, total, allKnown, currency
}

// buildToolBuckets maps each interactive-toolkit usage bucket to a priced
// ToolCostBucket. Tool cost is always provider-reported (already micro-USD and
// "known"), so there is no table lookup here — this is a pure projection plus a
// sum. Returns the buckets in caller order and their total.
func buildToolBuckets(usages []pipeline.ToolUsage) (buckets []v1.ToolCostBucket, total int64) {
	if len(usages) == 0 {
		return nil, 0
	}
	buckets = make([]v1.ToolCostBucket, len(usages))
	for i, u := range usages {
		buckets[i] = v1.ToolCostBucket{Tool: u.Tool, AmountMicroUSD: u.CostMicroUSD, PricingKnown: u.CostReported}
		total += u.CostMicroUSD
	}
	return buckets, total
}

func costNotice(e *pipeline.SessionEndInfo, c v1.EstimatedSessionCost, known bool) *notice.Notice {
	if !known {
		return notice.New(categories.SessionCost, notice.Args{
			Lead: "Session cost unavailable",
			Body: fmt.Sprintf("No pricing is configured for %s.", e.Model),
			Fields: []channelevents.InteractionField{{
				Label: "Tokens",
				Value: fmt.Sprintf("%s in / %s out", formatTokens(e.InputTokens), formatTokens(e.OutputTokens)),
			}},
			Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
		})
	}
	fields := []channelevents.InteractionField{
		{Label: "Tokens", Value: fmt.Sprintf("%s in / %s out",
			formatTokens(e.InputTokens), formatTokens(e.OutputTokens))},
		{Label: "Cache", Value: fmt.Sprintf("%s read / %s write",
			formatTokens(e.CacheReadTokens), formatTokens(e.CacheCreationTokens))},
		{Label: "Model", Value: e.Model},
	}
	// Break out inner interactive-toolkit spend (e.g. a passthrough `claude`
	// sub-run) so the total's provenance is visible, not folded silently.
	var toolTotal int64
	for _, b := range c.ByTool {
		toolTotal += b.AmountMicroUSD
	}
	if toolTotal > 0 {
		fields = append(fields, channelevents.InteractionField{
			Label: "Sub-agent tools", Value: formatUSD(toolTotal),
		})
	}
	return notice.New(categories.SessionCost, notice.Args{
		Lead:     fmt.Sprintf("This session cost ~%s", formatUSD(c.AmountMicroUSD)),
		Fields:   fields,
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}

var _ pipeline.Hook = (*Reporter)(nil)
