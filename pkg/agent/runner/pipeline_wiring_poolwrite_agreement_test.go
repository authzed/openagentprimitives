package runner

// This file pins the CROSS-TASK contract the write path depends on: the
// info-leakage audience gate (pkg/authz/hooks) and record_observation's own
// Execute (pkg/agent/tool/meta) must read the SAME destination out of the
// SAME raw call bytes. The two are independent implementations in different
// packages — the gate via resolveToolArgAsExecuted, the tool via
// tool.ParseArgs into a flat struct — and a prior round found they could be
// aimed at different pools: a payload with a top-level `resource` and a
// diverging nested `args.resource` had the gate judging one pool while the
// tool wrote to the other (see deps.go's resolveToolArgAsExecuted doc). That
// is now refused outright, but "refused outright" is itself a claim about
// agreement between the two readers, not just about the gate's own parsing —
// so this file runs the REAL gate and the REAL tool over identical bytes and
// checks they agree, rather than re-testing either one's parsing in
// isolation.
//
// Neither resolveToolArgAsExecuted nor ExtractToolIDArg is reachable from
// this package (both are unexported in pkg/authz/hooks), so the gate's
// resolved destination is observed the only way a caller outside that
// package can: through hooks.InfoLeakAudience.Eval's own exported Decision,
// whose refusal text embeds the destination it resolved (poolDestination.
// Describe() via refuseTheShare) — which is also exactly what an operator or
// approver reading that refusal would see, so the same string being right
// here is not a test-only convenience.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// fakeAgreementPoolWriter is record_observation's sess.Mem double for this
// file: Query/Search are unused stubs, and PutToPool records exactly what
// pool Execute handed it.
type fakeAgreementPoolWriter struct {
	put func(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error)
}

func (f fakeAgreementPoolWriter) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return memory.QueryResult{}, nil
}

func (f fakeAgreementPoolWriter) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}

func (f fakeAgreementPoolWriter) PutToPool(ctx context.Context, sessionScope, poolScope memory.Scope, e memory.Entry) (memory.Entry, error) {
	return f.put(ctx, sessionScope, poolScope, e)
}

// TestRecordObservationAgreesWithTheGateOnTheWriteDestination is the positive
// case: an ordinary, well-formed call (no envelope, one `resource` field) —
// exactly the shape record_observation's flat InputSchema asks a model to
// send. Runs the REAL production declaration (l.memoryPoolWritesDecl,
// unmodified) through the REAL exported gate, and the REAL tool, over the
// SAME json.RawMessage, and requires they name the same pool.
//
// The fixture (a prior read of an unrelated resource, permitted only to a
// different subject than the write destination's own audience) exists to
// make the gate actually DENY rather than allow vacuously — an Allow proves
// nothing about which destination it resolved.
func TestRecordObservationAgreesWithTheGateOnTheWriteDestination(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns-a", Name: "sess-a"}}

	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:         "enforcing",
		LookupWrites: l.memoryPoolWritesDecl,
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return []infoleakagetaint.TaintRecord{{ResourceType: "customer", ResourceID: "acme", Permission: "view"}}, nil
		},
		LookupSubjects: func(_ context.Context, resource, _ string) ([]string, error) {
			if resource == "customer:acme" {
				return []string{"user:alice"}, nil
			}
			// Whatever the WRITE destination turns out to be, its audience is a
			// different subject than the taint's — the fixture that forces a
			// real refusal rather than a vacuous allow.
			return []string{"user:carol"}, nil
		},
	})

	raw := json.RawMessage(`{"resource":"dossier:d-1","text":"prefers async review"}`)

	decision := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: meta.RecordObservationToolName, Args: raw},
	})
	require.Equal(t, pipeline.Deny, decision.Verdict,
		"fixture must actually exercise the gate's refusal path, or this test proves nothing about which pool it resolved")

	var gotPool memory.Scope
	sess := &tool.SessionContext{Namespace: "ns-a", Name: "sess-a", Mem: fakeAgreementPoolWriter{
		put: func(_ context.Context, _, pool memory.Scope, e memory.Entry) (memory.Entry, error) {
			gotPool = pool
			return e, nil
		},
	}}

	res, err := meta.NewRecordObservation().Execute(context.Background(), raw, sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool result: %s", res.Content)

	assert.Equal(t, "resource", gotPool.Kind)
	assert.Contains(t, decision.Reason, gotPool.ID,
		"the gate's own refusal must name the EXACT pool the tool actually wrote to — a divergence here is "+
			"the gate judging a different write than the one that happened")
}

// TestRecordObservationAndTheGateReadTheSameFieldOnAnAmbiguousPayload pins
// the other half of the contract: a call naming its destination twice,
// disagreeing, is refused by the gate OUTRIGHT (fix round 2 of Task 4)
// rather than silently resolved one way or the other. This call never
// reaches record_observation's Execute in production — dispatchToolUses runs
// PreToolCall before Execute, and a Deny here stops the dispatch — so the
// second half of this test (running the tool directly on the same bytes) is
// not exercising a reachable path. It exists to show that if the gate's
// refusal were ever weakened to a "pick one" resolution instead of an outright
// deny, the tool's own reading (top-level, since no operation_id envelope is
// present) is the SAME rule resolveToolArgAsExecuted already prefers in that
// case — so the two would keep agreeing by construction, not by accident.
func TestRecordObservationAndTheGateReadTheSameFieldOnAnAmbiguousPayload(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "ns-a", Name: "sess-a"}}
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:         "enforcing",
		LookupWrites: l.memoryPoolWritesDecl,
	})

	raw := json.RawMessage(`{"resource":"ledger:l-9","text":"x","args":{"resource":"dossier:d-1"}}`)

	decision := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: meta.RecordObservationToolName, Args: raw},
	})
	require.Equal(t, pipeline.Deny, decision.Verdict,
		"a call naming its destination twice, disagreeing, must be refused outright rather than resolved either way")
	assert.Contains(t, decision.Reason, "named twice")

	var gotPool memory.Scope
	sess := &tool.SessionContext{Namespace: "ns-a", Name: "sess-a", Mem: fakeAgreementPoolWriter{
		put: func(_ context.Context, _, pool memory.Scope, e memory.Entry) (memory.Entry, error) {
			gotPool = pool
			return e, nil
		},
	}}
	res, err := meta.NewRecordObservation().Execute(context.Background(), raw, sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "tool result: %s", res.Content)
	assert.Equal(t, "ledger:l-9", gotPool.ID,
		"the tool reads the TOP-LEVEL field, exactly as resolveToolArgAsExecuted does for a call with no "+
			"operation_id envelope — never the nested decoy. Unreachable in production: the gate above denies "+
			"this exact call first")
}
