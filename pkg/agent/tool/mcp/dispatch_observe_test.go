package mcp_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
)

// prPayload is what `gh pr view <n> --json …` returns, in the single-text-block
// shape newTextToolServer serves — the same shape the relwrites tests use.
const prPayload = `{"number":6,"headRefOid":"ba03f5969a","isCrossRepository":false,` +
	`"headRepository":{"nameWithOwner":"demo-org/demo-repo"}}`

// prObserves is the two-subject block from the design: the head commit and the
// pull request, co-derived with the fact, all from one result.
func prObserves() []spiceboxv1alpha1.ObservesBlock {
	return []spiceboxv1alpha1.ObservesBlock{{
		ForEach: "[result]",
		Subjects: []spiceboxv1alpha1.ObserveSubject{
			{ResourceType: `"git_commit"`, ResourceID: `item.headRefOid`},
			{ResourceType: `"github_pr"`, ResourceID: `item.headRepository.nameWithOwner + "#" + string(item.number)`},
		},
		Facts: map[string]string{"is_cross_repository": `item.isCrossRepository`},
	}}
}

// newObserveMemory builds the in-memory facade the same way Task 2's
// factcontent/accessor_test.go newMem does, and wires it onto mt through the
// same setter the neighbouring relwritesaudit.Record call reaches m.mem
// through (mt.SetMemory) — dispatch.go has no other path to a tool's memory
// handle.
//
// The scope's ID is "/" because newOpAndSess's SessionContext always has an
// empty Namespace and Name, and dispatch.go derives the memory scope as
// `sess.Namespace + "/" + sess.Name` — this must match that expression
// exactly for the ForSubject reads below to land on what Execute wrote.
func newObserveMemory(t *testing.T, mt *mcp.MCPTool) (memory.Memory, memory.Scope) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "/"}
	mt.SetMemory(mem)
	return mem, scope
}

// readCtx is a ReadMemory-approved ctx for the test's OWN read-back
// assertions. A bare context.Background() cannot pass Local.Query's
// unconditional ReadMemory door (facade.go:516) — the same door RULING T2-D
// requires dispatch.go itself to clear before calling observedfact.Record.
// Same shape as factcontent/accessor_test.go's newMem.
func readCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

func TestExecute_RecordsObservedFactsOnSuccess(t *testing.T) {
	// mustSynthesize's CR always declares tool "search_issues" (see its own
	// doc comment) — the server must expose the same name or the go-sdk
	// client's tools/call never reaches it.
	srv, _ := newTextToolServer(t, "search_issues", prPayload, false)
	tools := mustSynthesize(t, srv.URL)
	mt := tools[0].(*mcp.MCPTool)

	mem, scope := newObserveMemory(t, mt)
	mt.SetObserves(prObserves())

	_, opID, sess := newOpAndSess(t)
	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError; content=%q", res.Content)

	// Both subjects the SAME result named carry the fact.
	commit, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "git_commit", "ba03f5969a")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, commit)

	pr, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr)

	// And nothing about a PR this result did not name. This is the laundering
	// case at the dispatch layer: observing #6 must say nothing about #5.
	other, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#5")
	require.NoError(t, err)
	assert.Empty(t, other)
}

// TestExecute_ObservesSeesTheSessionBinding pins that this dispatch path binds
// `session`, which pkg/authz/observe's package doc advertises as available and
// which the sandbox path has always bound (sandboxCELVars).
//
// Without it the failure is quiet in the worst way: relwrites nil-FILLS an
// absent binding rather than rejecting it, so a block referencing `session`
// records a null value here while working correctly on a sandbox tool. An
// observes block is meant to be portable between the two, and nothing else
// would report that it is not.
func TestExecute_ObservesSeesTheSessionBinding(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prPayload, false)
	tools := mustSynthesize(t, srv.URL)
	mt := tools[0].(*mcp.MCPTool)

	mem := memory.NewLocal(inmem.NewBackend())
	mt.SetMemory(mem)

	// A real namespace/name, unlike newObserveMemory's "/" scope, so the
	// asserted value is the session's actual identity rather than a string
	// that would also be produced by two empty fields.
	_, opID, sess := newOpAndSess(t)
	sess.Namespace, sess.Name = "default", "demo-session"
	scope := memory.Scope{Kind: "session", ID: "default/demo-session"}

	mt.SetObserves([]spiceboxv1alpha1.ObservesBlock{{
		ForEach: "[result]",
		Subjects: []spiceboxv1alpha1.ObserveSubject{
			{ResourceType: `"github_pr"`, ResourceID: `item.headRepository.nameWithOwner + "#" + string(item.number)`},
		},
		Facts: map[string]string{"observed_in_session": `session`},
	}})

	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError; content=%q", res.Content)

	got, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"observed_in_session": "default/demo-session"}, got,
		"`session` must resolve to the dispatching session, not nil")
}

func TestExecute_SkipsObservesOnError(t *testing.T) {
	// isError=true on the tools/call response: the call asserted nothing, so it
	// records nothing. Mirrors TestExecute_SkipsWritesOnError.
	srv, _ := newTextToolServer(t, "search_issues", prPayload, true)
	tools := mustSynthesize(t, srv.URL)
	mt := tools[0].(*mcp.MCPTool)

	mem, scope := newObserveMemory(t, mt)
	mt.SetObserves(prObserves())

	_, opID, sess := newOpAndSess(t)
	_, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err)

	got, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Empty(t, got, "a failed call asserts nothing")
}

func TestExecute_ObserveEvaluationFailureIsSurfacedNotSwallowed(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prPayload, false)
	tools := mustSynthesize(t, srv.URL)
	mt := tools[0].(*mcp.MCPTool)

	mem, scope := newObserveMemory(t, mt)
	bad := prObserves()
	bad[0].Subjects[0].ResourceID = `""` // resolves empty: unnameable subject

	mt.SetObserves(bad)
	_, opID, sess := newOpAndSess(t)
	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute itself does not error; the failure rides on the result")
	assert.True(t, res.IsError, "a fact that could not be recorded must not look like success")
	assert.Contains(t, res.Content, "record")

	// Neither subject was recorded: a block-level evaluation failure must be
	// all-or-nothing, not "the subject before the bad expression got written."
	commit, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "git_commit", "ba03f5969a")
	require.NoError(t, err)
	assert.Empty(t, commit)

	pr, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Empty(t, pr)
}

func TestExecute_ContradictionIsRefusedLoudly(t *testing.T) {
	// Second call, same subjects, DIFFERENT value: the append-only door refuses
	// it and the agent is told. A silent overwrite here is the bypass.
	srv, _ := newTextToolServer(t, "search_issues", prPayload, false)
	tools := mustSynthesize(t, srv.URL)
	mt := tools[0].(*mcp.MCPTool)
	mem, scope := newObserveMemory(t, mt)
	mt.SetObserves(prObserves())

	_, opID, sess := newOpAndSess(t)
	_, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err)

	// Re-point the tool at a server returning the opposite verdict for the
	// same PR and head commit.
	flipped := `{"number":6,"headRefOid":"ba03f5969a","isCrossRepository":true,` +
		`"headRepository":{"nameWithOwner":"demo-org/demo-repo"}}`
	srv2, _ := newTextToolServer(t, "search_issues", flipped, false)
	tools2 := mustSynthesize(t, srv2.URL)
	mt2 := tools2[0].(*mcp.MCPTool)
	// Point mt2 at the SAME memory facade the first call used (not a fresh
	// one) — the two tools must share one append-only namespace for this to
	// prove a real collision rather than two isolated scopes that could never
	// conflict.
	mt2.SetMemory(mem)
	mt2.SetObserves(prObserves())

	res, err := mt2.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError, "a contradiction must be visible, not silently dropped")

	// The original value stands.
	pr, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr, "the first fact wins; it is write-once")
}

// TestExecute_RepeatObservationIsNotAnError is RULING T2-D's test: the
// SECOND call, with IDENTICAL args against the SAME server (an agent calling
// `gh pr view 6` twice), must not surface as an error.
//
// observedfact.Record's repeat-observation path re-derives a new
// ObservationID each call (see factcontent.Record's RULING T1-A comment), so
// the facade's append-only Put sees a non-equivalent entry on the second
// write and answers ErrAppendOnlyConflict; Record then reads the stored
// value back to tell that apart from a genuine contradiction — and that
// read-back is a Query, which Local.Query gates behind ReadMemory
// unconditionally (facade.go:516). Without the WithSystemApproval wrap in
// dispatch.go, this ordinary re-observation would fail with an error
// wrapping ErrAppendOnlyConflict instead of the no-op it actually is — this
// test is what would catch that regression; nothing else in this file
// exercises the SAME tool called twice with the SAME result.
func TestExecute_RepeatObservationIsNotAnError(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prPayload, false)
	tools := mustSynthesize(t, srv.URL)
	mt := tools[0].(*mcp.MCPTool)
	mem, scope := newObserveMemory(t, mt)
	mt.SetObserves(prObserves())

	_, opID, sess := newOpAndSess(t)

	first, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute (first call)")
	require.False(t, first.IsError, "unexpected IsError on first call; content=%q", first.Content)

	second, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute (second call)")
	assert.False(t, second.IsError, "a repeat observation of the same value must be a no-op, not an error; content=%q", second.Content)

	pr, err := factcontent.ForSubject(readCtx(), mem, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, pr)
}
