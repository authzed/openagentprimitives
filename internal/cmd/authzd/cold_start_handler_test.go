package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/metaagentthread"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

type fakeColdStartExtractor struct {
	out scope.ColdStartExtraction
	err error
}

func (f *fakeColdStartExtractor) ExtractColdStart(_ context.Context, _ ExtractorInput) (scope.ColdStartExtraction, error) {
	return f.out, f.err
}

func newColdStartHarness(t *testing.T, ext coldStartExtractorIface) (*ColdStartHandler, memory.Memory, memory.Scope) {
	t.Helper()
	m := memory.NewLocal(inmem.NewBackend())
	mg := &Metaagent{Memory: m, Extractor: &fakeMetaExtractor{}, Composer: &fakeMetaComposer{out: ComposerOutput{ApproverSummary: "scoped."}}}
	h := &ColdStartHandler{Metaagent: mg, Extractor: ext}
	return h, m, memory.Scope{Kind: "session", ID: "ns/a"}
}

// coldStartToolDisallowed reports whether the Layer-2 session_scope memory doc
// records tool in its tool-deny set — where ApplyDelta puts hard-denied tools,
// and where the runner's Scope hook reads them at dispatch.
func coldStartToolDisallowed(t *testing.T, m memory.Memory, sc memory.Scope, tool string) bool {
	t.Helper()
	got, found, err := sessionscope.Get(approvedCtx(), m, sc)
	require.NoError(t, err)
	if !found {
		return false
	}
	for _, d := range got.Tools.Deny {
		if d == tool {
			return true
		}
	}
	return false
}

func coldStartHasNotice(t *testing.T, m memory.Memory, sc memory.Scope) bool {
	t.Helper()
	msgs, err := metaagentthread.List(approvedCtx(), m, sc)
	require.NoError(t, err)
	for _, msg := range msgs {
		if msg.Role == metaagentthread.RoleMetaagentNotice {
			return true
		}
	}
	return false
}

func coldStartReq() ColdStartRequest {
	return ColdStartRequest{
		Requester: "user:alice", RequestText: "summarize L-140 and do not read ENG",
		Envelope: scope.AgentClassEnvelope{
			BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "linear_issue"}},
			Tools:         []scope.EnvelopeTool{{Name: "linear.search_issues"}},
		},
		RequesterPerms: scope.RequesterPerms{AllowedIDsByType: map[string]map[string]bool{"linear_issue": {}}},
		InboxIdx:       0,
	}
}

func TestColdStart_ApproveCleaned_AppliesScope_WritesCleaned(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		CleanedTask: "summarize L-140",
	}}
	h, m, sc := newColdStartHarness(t, ext)
	err := h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			return ColdStartApproveCleaned, nil
		})
	require.NoError(t, err)
	cst, found, _ := coldstarttask.Get(approvedCtx(), m, sc)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	assert.Equal(t, "summarize L-140", cst.CleanedText)
	assert.True(t, coldStartToolDisallowed(t, m, sc, "linear.search_issues"), "hard-deny tool lands in Layer-2 memory tool-deny")
	assert.True(t, coldStartHasNotice(t, m, sc), "approve must post a confirmation notice")
}

// TestColdStart_NoScopeChange_NoOpRunsTaskWithoutApproval verifies the no-op
// fast path: when the extractor finds no permission change, the handler does NOT
// prompt the approver — it writes approved_cleaned with the task so the agent
// starts immediately, and applies no scope.
func TestColdStart_NoScopeChange_NoOpRunsTaskWithoutApproval(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{CleanedTask: "summarize L-140"}} // empty ScopeDelta
	h, m, sc := newColdStartHarness(t, ext)
	err := h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			t.Fatal("decider must NOT be called when there is no scope change")
			return "", nil
		})
	require.NoError(t, err)
	cst, found, _ := coldstarttask.Get(approvedCtx(), m, sc)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status, "no scope change → run the cleaned task")
	assert.Equal(t, "summarize L-140", cst.CleanedText)
	_, scopeFound, _ := sessionscope.Get(approvedCtx(), m, sc)
	assert.False(t, scopeFound, "no scope change → no session_scope doc written")
}

// TestColdStart_NoScopeChange_NoTask_RunsOriginal verifies the no-op path falls
// back to approved_original when there is neither a scope change nor a cleaned
// task to strip (the agent runs the raw turn).
func TestColdStart_NoScopeChange_NoTask_RunsOriginal(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{}} // empty delta + empty task
	h, m, sc := newColdStartHarness(t, ext)
	require.NoError(t, h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			t.Fatal("decider must NOT be called when there is no scope change")
			return "", nil
		}))
	cst, _, _ := coldstarttask.Get(approvedCtx(), m, sc)
	assert.Equal(t, coldstarttask.StatusApprovedOriginal, cst.Status)
	_, scopeFound, _ := sessionscope.Get(approvedCtx(), m, sc)
	assert.False(t, scopeFound, "no scope change → no session_scope doc written")
}

func TestColdStart_ApproveOriginal_AppliesScope_StatusOriginal(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}, CleanedTask: "summarize L-140",
	}}
	h, m, sc := newColdStartHarness(t, ext)
	require.NoError(t, h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			return ColdStartApproveOriginal, nil
		}))
	cst, _, _ := coldstarttask.Get(approvedCtx(), m, sc)
	assert.Equal(t, coldstarttask.StatusApprovedOriginal, cst.Status)
	assert.Empty(t, cst.CleanedText, "approve-original leaves CleanedText empty; runner uses raw turn")
	assert.True(t, coldStartToolDisallowed(t, m, sc, "linear.search_issues"), "scope still applied on approve-original")
}

func TestColdStart_RunWithoutScope_NoApply(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}, CleanedTask: "summarize L-140",
	}}
	h, m, sc := newColdStartHarness(t, ext)
	require.NoError(t, h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			return ColdStartRunWithoutScope, nil
		}))
	cst, _, _ := coldstarttask.Get(approvedCtx(), m, sc)
	assert.Equal(t, coldstarttask.StatusRanWithoutScope, cst.Status)
	_, scopeFound, _ := sessionscope.Get(approvedCtx(), m, sc)
	assert.False(t, scopeFound, "no scope applied → session_scope not written")
	assert.True(t, coldStartHasNotice(t, m, sc), "run-without-scope must post a confirmation notice")
}

func TestColdStart_Deny(t *testing.T) {
	// Non-empty delta so the decider runs (an empty delta hits the no-op path).
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}, CleanedTask: "summarize L-140",
	}}
	h, m, sc := newColdStartHarness(t, ext)
	require.NoError(t, h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) { return ColdStartDeny, nil }))
	cst, _, _ := coldstarttask.Get(approvedCtx(), m, sc)
	assert.Equal(t, coldstarttask.StatusDenied, cst.Status)
	_, scopeFound, _ := sessionscope.Get(approvedCtx(), m, sc)
	assert.False(t, scopeFound, "cold-start deny applies no scope")
	assert.True(t, coldStartHasNotice(t, m, sc), "deny must post a requester notice (no silent error)")
}

func TestColdStart_ExtractorError_FailsClosed(t *testing.T) {
	ext := &fakeColdStartExtractor{err: assertError("llm down")}
	h, m, sc := newColdStartHarness(t, ext)
	err := h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			t.Fatal("decider must not be called on extractor failure")
			return "", nil
		})
	// The extractor error must propagate (no silent swallow) so the worker logs it.
	require.Error(t, err, "extractor failure must surface, not be swallowed")
	assert.Contains(t, err.Error(), "llm down", "the underlying extractor error must be wrapped")
	cst, found, _ := coldstarttask.Get(approvedCtx(), m, sc)
	require.True(t, found)
	assert.Equal(t, coldstarttask.StatusScopeReviewFailed, cst.Status,
		"fail closed: the runner must halt, NOT run the raw task unscoped")
	assert.True(t, coldStartHasNotice(t, m, sc), "extractor-failure must post a requester notice")
}

func TestColdStart_AutoApply_NoDecider(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}, CleanedTask: "summarize L-140",
	}}
	h, m, sc := newColdStartHarness(t, ext)
	req := coldStartReq()
	req.AutoApply = true
	require.NoError(t, h.Handle(approvedCtx(), sc, "ns/a", req,
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			t.Fatal("decider must not be called in autoApply")
			return "", nil
		}))
	cst, _, _ := coldstarttask.Get(approvedCtx(), m, sc)
	assert.Equal(t, coldstarttask.StatusApprovedCleaned, cst.Status)
	assert.True(t, coldStartToolDisallowed(t, m, sc, "linear.search_issues"), "autoApply must apply scope to Layer-2 memory")
}

func TestColdStart_UnknownAction_Errors(t *testing.T) {
	// Non-empty delta so the decider runs (an empty delta hits the no-op path).
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}, CleanedTask: "x",
	}}
	h, _, sc := newColdStartHarness(t, ext)
	err := h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) { return "bogus", nil })
	require.Error(t, err)
}

func TestColdStart_WritesAuditRecord(t *testing.T) {
	ext := &fakeColdStartExtractor{out: scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
		CleanedTask: "summarize L-140",
	}}
	h, m, sc := newColdStartHarness(t, ext)
	require.NoError(t, h.Handle(approvedCtx(), sc, "ns/a", coldStartReq(),
		func(context.Context, scope.MetaagentOutput, string) (string, error) {
			return ColdStartApproveCleaned, nil
		}))

	records, err := metaagentaudit.List(approvedCtx(), m, sc)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, ColdStartApproveCleaned, records[0].ApproverDecision)
	assert.Equal(t, "user:alice", records[0].Requester)
	assert.NotNil(t, records[0].AppliedDelta, "AppliedDelta must be set on approve")
}
