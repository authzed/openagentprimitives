package runner

// This file answers, by running code, the question the test it replaces only
// claimed to answer: does record_observation reach the contained dispatch
// pipeline, and does the pipeline's first gate LET IT THROUGH when it gets
// there?
//
// The replaced test (meta's TestRecordObservationRoutesThroughThePipeline)
// asserted `StateImpact == Readwrite && Check == nil` — the exact pair that
// makes routing end in a deny. Readonly and Readwrite share one arm in
// toolcheck.Checker.check, and that arm refuses a nil Check outright; needsApproval
// returns false for it, so no card is raised and the call is simply denied at
// OrderToolCallAuthz (20), before InfoLeakAudience (50) — the gate the routing
// existed to reach — ever runs. A declaration assertion cannot see any of that,
// which is why this file runs the REAL checker and the REAL hook instead.
//
// Two halves, both required:
//
//   - ROUTING is asked through gatesThroughPipeline, the same function
//     dispatchToolUses calls, rather than a transcription of its expression.
//   - The VERDICT is taken from hooks.ToolCallAuthz over toolcheck.Checker with
//     a NIL SpiceDB client, in enforcing and permissive. A nil client is
//     fail-closed for anything that reaches SpiceDB, so an Allow here can only
//     have come from the trivial-permission arm — it cannot be a permissive
//     bypass or an accidentally-satisfied check.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// toolCallAuthzOverRealChecker builds the production hook over the production
// checker: hooks.NewToolCallAuthz with a toolcheck.Checker whose SpiceDB client
// is nil. Nothing is faked between the tool's declaration and the verdict.
func toolCallAuthzOverRealChecker(t *testing.T, mode string, perm authz.Permission) *hooks.ToolCallAuthz {
	t.Helper()
	return hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    mode,
		Checker: toolcheck.Checker{},
		ResolvePermission: func(string, map[string]any) (authz.Permission, error) {
			return perm, nil
		},
		BuildInputs: func(pipeline.Input, map[string]any, authz.Permission) authz.Inputs {
			return authz.Inputs{}
		},
	})
}

// TestRecordObservationRoutesThroughThePipelineAndIsAllowedThere is the pin B1
// asked for: routing proved through the dispatcher's own predicate, and the
// gate at the other end proved to ALLOW rather than to deny.
func TestRecordObservationRoutesThroughThePipelineAndIsAllowedThere(t *testing.T) {
	tl := meta.NewRecordObservation()
	perm := tl.Permission()

	// The dispatcher's own predicate, called — not transcribed.
	require.True(t, gatesThroughPipeline(tl, perm),
		"record_observation must reach the contained pipeline, or the PreToolCall info-leakage audience gate never sees the write")

	// And it must get there by the ROUTING marker, not by a check-requiring
	// StateImpact: the latter is what the checker denies below.
	assert.True(t, pipelineRouted(tl), "routing must come from the tool.PipelineRouted marker")
	assert.False(t, perm.StateImpact.CheckRequired(),
		"a check-requiring StateImpact with a nil Check is denied by toolcheck.Checker on every call — the marker exists so routing does not have to claim one")

	raw := json.RawMessage(`{"resource":"dossier:d-1","text":"prefers async review"}`)
	for _, mode := range []string{"enforcing", "permissive"} {
		t.Run(mode+": ToolCallAuthz allows over the real checker", func(t *testing.T) {
			d := toolCallAuthzOverRealChecker(t, mode, perm).Eval(context.Background(), pipeline.Input{
				Point: pipeline.PreToolCall,
				Tool:  &pipeline.ToolCallInfo{Name: meta.RecordObservationToolName, Args: raw},
			})
			assert.Equal(t, pipeline.Allow, d.Verdict,
				"mode=%s: the tool-call gate must not refuse a tool whose authorization is not its to make; reason=%q", mode, d.Reason)
			assert.Empty(t, d.Reason, "mode=%s", mode)
			assert.Nil(t, d.Approval,
				"mode=%s: no human card either — a pool write is authorized by the write grant, not by an approver", mode)
		})
	}
}

// TestPipelineRoutingIsWhatCarriesRecordObservation removes the marker's only
// alternative explanation. A meta tool with a trivial permission and no marker
// takes the UNGATED path; the same tool with the marker takes the contained
// one. Without this, a future change that made every meta tool gated would keep
// the test above green while the marker did nothing.
func TestPipelineRoutingIsWhatCarriesRecordObservation(t *testing.T) {
	trivial := authz.Permission{StateImpact: authz.Stateless}
	assert.False(t, gatesThroughPipeline(unmarkedMetaTool{}, trivial),
		"an ordinary meta tool with a trivial permission must still take the ungated path")
	assert.True(t, gatesThroughPipeline(meta.NewRecordObservation(), trivial),
		"the marker — and only the marker — is what puts record_observation on the contained path")
}

// unmarkedMetaTool is a KindMeta tool carrying neither marker: the control case
// for the assertion above.
type unmarkedMetaTool struct{ tool.Tool }

func (unmarkedMetaTool) Kind() tool.Kind { return tool.KindMeta }

// TestRecordObservationDoesNotTaintItsOwnSessionAtPostToolCall closes the
// consequence routing brings with it, and which nothing else in the branch
// exercises: the tool now reaches the CONTAINED pipeline, so info_leak_read
// sees it at PostToolCall, and an UNDECLARED tool there is floored with an
// agentsession#unknown_provenance taint.
//
// That floor is not inert. The PreToolCall pool-write gate measures the
// session's accumulated taint on the NEXT call, so the tool's own first write
// would refuse its second — a tool that works exactly once per session. Both
// halves are asserted here, over the real hooks and the real production
// declarations, because the first alone (no taint appended) would keep passing
// if the floor moved somewhere else.
func TestRecordObservationDoesNotTaintItsOwnSessionAtPostToolCall(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns-a", Name: "sess-a"}}

	var appended []infoleakagetaint.TaintRecord
	rd := l.infoLeakReadDeps()
	rd.Mode = "enforcing"
	rd.AppendTaint = func(_ context.Context, r infoleakagetaint.TaintRecord) error {
		appended = append(appended, r)
		return nil
	}
	post := hooks.NewInfoLeakRead(rd).Eval(context.Background(), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name:   meta.RecordObservationToolName,
			Args:   json.RawMessage(`{"resource":"dossier:d-1","text":"prefers async review"}`),
			Result: "recorded",
		},
	})
	require.Equal(t, pipeline.Allow, post.Verdict)
	assert.Empty(t, appended,
		"a write brings no data IN, so nothing may be tainted for it — the undeclared-tool floor would taint the session itself")
	assert.Empty(t, post.Audit, "nor may it be recorded as an unmapped tool that was floored")

	// The half that says why the line above matters: a second write, measured
	// against whatever the first one left behind, must still be allowed.
	second := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:         "enforcing",
		LookupWrites: l.memoryPoolWritesDecl,
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return appended, nil
		},
		LookupSubjects: func(_ context.Context, resource, _ string) ([]string, error) {
			// The session's own participants are NOT the pool's audience —
			// which is exactly why a floored self-taint refuses the next write.
			if resource == "agentsession:ns-a/sess-a" {
				return []string{"user:alice"}, nil
			}
			return []string{"user:alice", "user:bob"}, nil
		},
	}).Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name: meta.RecordObservationToolName,
			Args: json.RawMessage(`{"resource":"dossier:d-1","text":"a second, unrelated note"}`),
		},
	})
	assert.Equal(t, pipeline.Allow, second.Verdict,
		"the second write into the same pool must not be refused by taint the first write minted about itself; reason=%q", second.Reason)
}
