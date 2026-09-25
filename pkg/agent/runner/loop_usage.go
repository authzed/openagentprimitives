package runner

import (
	"context"
	"log/slog"
	"math"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// sessionUsage is the cumulative token usage across all LLM calls in this
// run, kept on the Loop (not just loop-local) so fireSessionEnd can read it
// from any terminal call site (success, idle, l.fail). Guarded by usageMu.
type sessionUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// addUsage accumulates one LLM call's usage into the Loop's cumulative
// session snapshot. Safe for concurrent use.
func (l *Loop) addUsage(u llm.Usage) {
	l.usageMu.Lock()
	defer l.usageMu.Unlock()
	l.usage.InputTokens += u.InputTokens
	l.usage.OutputTokens += u.OutputTokens
	l.usage.CacheCreationTokens += u.CacheCreationTokens
	l.usage.CacheReadTokens += u.CacheReadTokens
}

// usageSnapshot returns a copy of the Loop's cumulative session usage.
// Safe for concurrent use.
func (l *Loop) usageSnapshot() sessionUsage {
	l.usageMu.Lock()
	defer l.usageMu.Unlock()
	return l.usage
}

// modelUsageBucket is one served model's running accumulation across the turns
// of this run. display is the uniform "<provider>/<model>" display id (the
// bucket key); model is the bare id used for pricing lookups, built from the
// same resp.Model/l.Model inputs as display rather than re-parsed out of it —
// see addModelUsage.
type modelUsageBucket struct {
	display string
	model   string

	inputTokens         int64
	outputTokens        int64
	cacheCreationTokens int64
	cacheReadTokens     int64

	reportedCostMicroUSD int64
	costReported         bool
}

// addModelUsage accumulates one LLM call's usage into the Loop's
// per-served-model running buckets, keyed by display id in
// first-encountered order. Guarded by usageMu (see its doc for why this
// shares the lock with addUsage rather than owning a second one).
func (l *Loop) addModelUsage(display, bareModel string, u llm.Usage) {
	l.usageMu.Lock()
	defer l.usageMu.Unlock()
	if l.usageByModelIdx == nil {
		l.usageByModelIdx = make(map[string]int)
	}
	i, ok := l.usageByModelIdx[display]
	if !ok {
		i = len(l.usageByModel)
		l.usageByModelIdx[display] = i
		l.usageByModel = append(l.usageByModel, modelUsageBucket{display: display, model: bareModel})
	}
	b := &l.usageByModel[i]
	b.inputTokens += u.InputTokens
	b.outputTokens += u.OutputTokens
	b.cacheCreationTokens += u.CacheCreationTokens
	b.cacheReadTokens += u.CacheReadTokens
	if u.CostUSD != nil {
		if *u.CostUSD < 0 {
			// A negative provider-reported cost would reduce the session total
			// stamped to status — either a buggy or a malicious upstream. Treat
			// this turn's cost as unreported (fall back to table pricing for the
			// bucket) rather than letting it net against real charges.
			slog.Default().Info("negative reported cost from provider; ignoring, falling back to table pricing",
				"display", display, "model", bareModel, "costUSD", *u.CostUSD)
		} else {
			// Round once per turn at the USD→micro-USD conversion so accumulation
			// across turns is pure int64 addition — no repeated float rounding.
			b.reportedCostMicroUSD += int64(math.Round(*u.CostUSD * 1e6))
			b.costReported = true
		}
	}
}

// usageByModelSnapshot returns the Loop's per-served-model usage as
// pipeline.ModelUsage, in first-encountered order, for fireSessionEnd to
// hand to SessionEndInfo.ByModel. Safe for concurrent use.
func (l *Loop) usageByModelSnapshot() []pipeline.ModelUsage {
	l.usageMu.Lock()
	defer l.usageMu.Unlock()
	if len(l.usageByModel) == 0 {
		return nil
	}
	out := make([]pipeline.ModelUsage, len(l.usageByModel))
	for i, b := range l.usageByModel {
		out[i] = pipeline.ModelUsage{
			Display:              b.display,
			Model:                b.model,
			InputTokens:          b.inputTokens,
			OutputTokens:         b.outputTokens,
			CacheCreationTokens:  b.cacheCreationTokens,
			CacheReadTokens:      b.cacheReadTokens,
			ReportedCostMicroUSD: b.reportedCostMicroUSD,
			CostReported:         b.costReported,
		}
	}
	return out
}

// StampEstimatedCost persists the cost estimate via the StatusPatcher. nil-safe:
// a Loop without a Status (tests/kubectl-driven) silently no-ops.
func (l *Loop) StampEstimatedCost(ctx context.Context, c spiceboxv1alpha1.EstimatedSessionCost) error {
	if l.Status == nil {
		return nil
	}
	return l.Status.PatchEstimatedCost(ctx, c)
}
