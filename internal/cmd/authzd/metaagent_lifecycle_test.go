package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeManageScope is a scripted authz.ManageScopeChecker for the staged
// MetaagentReceived gate (mid_session only).
type fakeManageScope struct {
	allowed bool
	calls   int
}

func (f *fakeManageScope) CheckManageScope(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	f.calls++
	return f.allowed, nil
}

// TestRunMetaagentLifecycle_ColdStart_ThreadsReceivedToApply drives a cold-start
// request through all four staged hooks over a real embedded NATS + orchestrator
// and asserts the end-to-end outcome: scope applied + the right cold_start_task.
// cold_start skips the manage_scope gate, so no checker is needed.
func TestRunMetaagentLifecycle_ColdStart_ThreadsReceivedToApply(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "life-cold"
	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: mem, Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "scoped."}}}
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		CleanedTask: "summarize L-140",
	}}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nc, &ColdStartHandler{Metaagent: mg, Extractor: ext})

	// Stand in for channelsd: approve the published cold-start request.
	approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
	sub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
		var p map[string]any
		if err := json.Unmarshal(m.Data, &p); err != nil {
			return
		}
		// cold-start payload carries coldStart:true (5-button).
		assert.Equal(t, true, p["coldStart"], "cold-start payload must carry coldStart:true")
		reqID, _ := p["requestId"].(string)
		if reqID == "" {
			return
		}
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, Action: ColdStartApproveCleaned, ApproverID: "user:bob"})
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	// The class's cold-start mode is read from the session's
	// authz_session_config snapshot (cold_start_policy.go); this class asks a
	// human, which is the round-trip above.
	require.NoError(t, asc.Snapshot(approvedCtx(), mem, scopeRef, asc.Content{
		Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndApprove",
	}))
	req := metaagentRequest{
		scopeRef:  scopeRef,
		requester: "user:alice",
		text:      "summarize L-140 and do not read ENG",
		coldStart: true,
		envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}},
			Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
		},
	}
	require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), req))

	cst, found, _ := coldstarttask.Get(approvedCtx(), mem, scopeRef)
	require.True(t, found, "Apply must write the cold_start_task")
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	assert.Equal(t, "summarize L-140", cst.CleanedText)
	got, scopeFound, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
	require.True(t, scopeFound, "Apply must apply the in-envelope hard-deny")
	assert.Contains(t, got.Tools.Deny, "linear.search_issues")
}

// TestRunMetaagentLifecycle_MidSession_OwnerApprove drives a mid-session request
// for the session OWNER (manage_scope allowed) through approve, asserting the
// widening lands and the published payload carries NO coldStart key (3-button).
func TestRunMetaagentLifecycle_MidSession_OwnerApprove(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "life-mid"
	mem := memory.NewLocal(inmem.NewBackend())
	// handleOne / the staged Extract pass no RequesterPerms (a documented
	// follow-on), so an Add would be dropped by ClassifySkipped. A HardDeny
	// passes the envelope-only check, so we drive a narrowing to exercise the
	// owner-approve apply path.
	mg := &Metaagent{
		Memory: mem,
		Extractor: &fakeMetaExtractor{result: scope.ScopeDelta{
			HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}},
		}},
		Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "Deny foo/bar."}},
	}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nc, nil)
	w.SetManageScopeChecker(&fakeManageScope{allowed: true})

	approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
	sub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
		var p map[string]any
		if err := json.Unmarshal(m.Data, &p); err != nil {
			return
		}
		_, hasColdStart := p["coldStart"]
		assert.False(t, hasColdStart, "mid-session payload must NOT carry coldStart (3-button)")
		reqID, _ := p["requestId"].(string)
		if reqID == "" {
			return
		}
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "user:bob"})
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	req := metaagentRequest{
		scopeRef:  scopeRef,
		requester: "user:alice",
		text:      "@metaagent do not read foo/bar",
		envelope:  scope.AgentClassEnvelope{BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}}},
	}
	require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), req))

	got, found, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
	require.True(t, found, "approve applies the hard-deny narrowing")
	assert.True(t, got.ResourceDisallowed("github_repo", "foo/bar"))
}

// TestRunMetaagentLifecycle_MidSession_NonOwner_ReceivedDenyShortCircuits proves
// a non-owner is denied at Received and NO extraction / scope mutation occurs.
func TestRunMetaagentLifecycle_MidSession_NonOwner_ReceivedDenyShortCircuits(t *testing.T) {
	const ns, name = "default", "life-deny"
	mem := memory.NewLocal(inmem.NewBackend())
	extractorCalled := false
	mg := &Metaagent{
		Memory: mem,
		Extractor: &fakeMetaExtractorFn{fn: func() scope.ScopeDelta {
			extractorCalled = true
			return scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"x"}}}
		}},
		Composer: &fakeMetaComposer{},
	}
	w := NewMetaagentWorker(mg, approval.New(), nil, nil)
	checker := &fakeManageScope{allowed: false}
	w.SetManageScopeChecker(checker)

	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	req := metaagentRequest{
		scopeRef:  scopeRef,
		requester: "user:bob",
		text:      "@metaagent do not read x",
		envelope:  scope.AgentClassEnvelope{Tools: []scope.EnvelopeTool{{Name: "x"}}},
	}
	require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), req))

	assert.Equal(t, 1, checker.calls, "the manage_scope gate is consulted for mid_session")
	assert.False(t, extractorCalled, "Received Deny must short-circuit before Extract runs the LLM")
	_, found, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
	assert.False(t, found, "a denied non-owner mutates no scope")
}

// TestRunMetaagentLifecycle_ColdStart_ExtractHaltShortCircuits proves an
// extractor error halts the sequence (fail-closed) and writes
// StatusScopeReviewFailed without any approval round-trip.
func TestRunMetaagentLifecycle_ColdStart_ExtractHaltShortCircuits(t *testing.T) {
	const ns, name = "default", "life-halt"
	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: mem, Composer: &fakeMetaComposer{}}
	ext := &fakeColdStartExtractor{err: fmt.Errorf("llm down")}
	w := NewMetaagentWorker(mg, approval.New(), nil, &ColdStartHandler{Metaagent: mg, Extractor: ext})

	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	// A resolvable cold-start policy, so the extractor error below is the only
	// thing that can fail the session closed.
	require.NoError(t, asc.Snapshot(approvedCtx(), mem, scopeRef, asc.Content{
		Subject: "alice", ScopeEnabled: true, ColdStart: "extractAndApprove",
	}))
	req := metaagentRequest{
		scopeRef:  scopeRef,
		requester: "user:alice",
		text:      "summarize L-140",
		coldStart: true,
		envelope:  scope.AgentClassEnvelope{},
	}
	require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), req))

	cst, found, _ := coldstarttask.Get(approvedCtx(), mem, scopeRef)
	require.True(t, found, "fail-closed writes the cold_start_task")
	assert.Equal(t, coldstarttask.StatusScopeReviewFailed, cst.Status,
		"extractor error fails the session closed (runner halts)")
	_, scopeFound, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
	assert.False(t, scopeFound, "no scope is applied on a fail-closed extract")
}

// TestRunMetaagentLifecycle_MidSession_Approve_EmitsScopeMutated verifies that
// a successful mid-session scope change emits a ScopeMutated event to the
// session's lifecycle log (in-flight, no phase change).
func TestRunMetaagentLifecycle_MidSession_Approve_EmitsScopeMutated(t *testing.T) {
	nc := startEmbeddedNATS(t)
	const ns, name = "default", "life-mutated"
	mem := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{
		Memory: mem,
		Extractor: &fakeMetaExtractor{result: scope.ScopeDelta{
			HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "corp/sec"}}},
		}},
		Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "Deny corp/sec."}},
	}
	orch := approval.New()
	w := NewMetaagentWorker(mg, orch, nc, nil)
	w.SetManageScopeChecker(&fakeManageScope{allowed: true})

	approvalSubj := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindMetaagentScopeApproval)
	sub, err := nc.Subscribe(approvalSubj, func(m *natsgo.Msg) {
		var p map[string]any
		if err := json.Unmarshal(m.Data, &p); err != nil {
			return
		}
		reqID, _ := p["requestId"].(string)
		if reqID == "" {
			return
		}
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "user:bob"})
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	scopeRef := memory.Scope{Kind: "session", ID: ns + "/" + name}
	req := metaagentRequest{
		scopeRef:  scopeRef,
		requester: "user:alice",
		text:      "@metaagent do not read corp/sec",
		envelope:  scope.AgentClassEnvelope{BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}}},
	}
	require.NoError(t, w.runMetaagentLifecycle(approvedCtx(), req))

	// The scope change must have been applied.
	got, found, _ := sessionscope.Get(approvedCtx(), mem, scopeRef)
	require.True(t, found, "approve applies the hard-deny narrowing")
	assert.True(t, got.ResourceDisallowed("github_repo", "corp/sec"))

	// A ScopeMutated event must appear in the lifecycle log.
	evts, err := lifecycle.Events(approvedCtx(), mem, scopeRef)
	require.NoError(t, err)
	var hasScopeMutated bool
	for _, ev := range evts {
		if _, ok := ev.(lifecyclecore.ScopeMutated); ok {
			hasScopeMutated = true
		}
	}
	assert.True(t, hasScopeMutated, "mid-session scope approve must emit ScopeMutated to the lifecycle log")
}
