package runner

import (
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// leakageGateApplies returns true when the read-side info-leakage gate
// should run for a tool of the given kind. Internal meta tools
// (new_operation, agent_complete, respond_to_user, etc.) bypass the
// gate — they don't bring external SpiceDB-keyed resources into the
// session context, so the toolResourceMap declaration model doesn't
// apply to them. Every non-meta kind is gated by default (fail-closed):
// MCP and sandbox tools can read user data and MUST be declared, and
// unknown future kinds inherit gating until proven safe.
func leakageGateApplies(kind tool.Kind) bool {
	return kind != tool.KindMeta
}

// planGateGoverned reports whether a tool opts into plan-gate governance via
// the tool.PlanGateGoverned marker (delegate). Such a tool must flow through
// the contained dispatch pipeline even when its permission is Stateless,
// because the plan-gate hook — the thing that governs it — runs only there.
// See loop_dispatch.go's gatePipeline and tool.PlanGateHandle.
func planGateGoverned(t tool.Tool) bool {
	g, ok := t.(tool.PlanGateGoverned)
	return ok && g.PlanGateGoverned()
}

// pipelineRouted reports whether a tool opts into the contained dispatch
// pipeline via the tool.PipelineRouted marker (record_observation). Like
// planGateGoverned it is a ROUTING answer, not an authorization one: the tool's
// dispatch permission stays trivial, and the hook that actually governs the
// call — here the PreToolCall info-leakage audience gate — runs only on the
// contained path. See tool.PipelineRouted for why that is a marker rather than
// a check-requiring StateImpact.
func pipelineRouted(t tool.Tool) bool {
	r, ok := t.(tool.PipelineRouted)
	return ok && r.PipelineRouted()
}

// gatesThroughPipeline is the dispatch predicate: does this call run inside the
// contained Pre → Execute → Post pipeline, or take the ungated meta path?
//
// Named here rather than left inline at its one call site so a test can ask the
// REAL question instead of re-transcribing the expression. A transcribed copy
// is how TestRecordObservationRoutesThroughThePipeline came to assert a
// declaration while claiming to prove routing.
//
// perm is the VARIANT-RESOLVED permission, not t.Permission(): the dispatcher
// resolves variants before this and the executor's ToolCallAuthz hook checks
// the same resolved value, so reading the declaration here would let the three
// disagree.
func gatesThroughPipeline(t tool.Tool, perm authz.Permission) bool {
	return leakageGateApplies(t.Kind()) || perm.StateImpact.CheckRequired() || planGateGoverned(t) || pipelineRouted(t)
}
