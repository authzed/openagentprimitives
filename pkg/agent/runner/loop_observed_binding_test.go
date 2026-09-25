package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcptool "github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/engine"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
)

// This file covers the ONE thing a pkg/authz unit test structurally cannot:
// that something in PRODUCTION calls the observed promotion. Task 4 registered
// the fill source and pkg/authz binds the candidate, and both were green while
// nothing ever produced a candidate — a slot declaring `fillFrom: [observed]`
// would have stayed empty forever with the whole suite passing. So the
// assertion here runs the real dispatch path: a real MCP tool, against a real
// MCP server, whose declared `observes` block records a real fact, and then
// asks whether the slot is BOUND — never calling PromoteObservedSlots itself.

// observedPRPayload is the `gh api repos/…/pulls/<n>` shape the design's own
// observes example walks: the head OID and whether the head lives on a fork,
// co-derived from one result.
const observedPRPayload = `{"number":6,"headRefOid":"ba03f5969a","isCrossRepository":false}`

// observedMCPTool synthesizes a REAL MCPTool against a real go-sdk MCP server
// whose one tool returns observedPRPayload, with the `observes` block carried
// on the CR — the production wiring path (Synthesize copies Spec.Tools[].
// Observes onto the tool), not the test-only SetObserves setter.
func observedMCPTool(t *testing.T, mem memory.Memory) agenttool.Tool {
	t.Helper()
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		Tools: map[string]mcptest.ToolHandler{
			"read_pr": mcptest.StaticTool(mcptest.TextResult(observedPRPayload, false)),
		},
	})
	t.Cleanup(srv.Close)

	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "gitforge"
	cr.Spec.Server.URL = srv.URL
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{
		Name:       "read_pr",
		Permission: &authz.Permission{StateImpact: authz.Passthrough},
		Observes: []spiceboxv1alpha1.ObservesBlock{{
			ForEach: "[result]",
			Subjects: []spiceboxv1alpha1.ObserveSubject{
				{ResourceType: `"git_commit"`, ResourceID: `item.headRefOid`},
			},
			Facts: map[string]string{"is_cross_repository": `item.isCrossRepository`},
		}},
	}}
	// The loopback httptest server is refused by the production SSRF-guarded
	// client, so inject a plain one — same as the mcp package's own tests.
	res, err := mcptool.Synthesize(cr, []probe.Tool{{Name: "read_pr"}}, mcptool.WithHTTPClient(http.DefaultClient))
	require.NoError(t, err, "Synthesize")
	require.Len(t, res.LLMTools, 1)

	mt := res.LLMTools[0].(*mcptool.MCPTool)
	// SetMemory is how internal/cmd/runner hands a synthesized MCP tool the
	// session's memory handle at session start; without it the dispatcher logs
	// "declared blocks skipped" and records nothing.
	mt.SetMemory(mem)
	return mt
}

// observedSlotClass declares a git_commit slot that ONLY an observation may
// fill — the design's fork gate, minus the precondition (Task 6).
func observedSlotClass() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Slots: []spiceboxv1alpha1.AuthzSlot{{
					ResourceType: "git_commit",
					Description:  "the tree a checkout may land on",
					Permission:   "read",
					FillFrom:     []string{"observed"},
				}},
			},
		},
	}
}

// observedBindingLoop wires a Loop the way production does for the parts under
// test: the real authz Engine over a real in-memory memory facade, an allow-all
// SpiceDB stand-in, and a recording relationship writer that stands in for the
// SpiceDB grant write.
func observedBindingLoop(t *testing.T, mem memory.Memory, w *observedRelWriter, tools []agenttool.Tool) *Loop {
	t.Helper()
	l := &Loop{
		Tools:      tools,
		AgentClass: observedSlotClass(),
		SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"},
		Engine: engine.New(engine.Deps{
			Memory:      mem,
			ToolChecker: observedAllowAll{},
			RelWriter:   w,
		}),
	}
	l.authSubject = identity.CanonicalFromTrusted("alice", "test fixture")
	// An empty registry: no hook denies, so the contained pipeline runs the
	// tool and returns its result. The gate itself is not what this test is
	// about — whether a fact BINDS is.
	loopWithInjectedExecutor(t, l, pipeline.NewRegistry())
	return l
}

type observedAllowAll struct{}

func (observedAllowAll) CheckToolCall(_ context.Context, _ authz.Permission, _ authz.Inputs) authz.Result {
	return authz.Result{Outcome: authz.OutcomeAllowed}
}

type observedRelWriter struct{ wrote []authz.Relation }

func (w *observedRelWriter) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	w.wrote = append(w.wrote, rels...)
	return nil
}
func (w *observedRelWriter) DeleteRelationships(_ context.Context, _ []authz.Relation) error {
	return nil
}

// dispatchObservingTool drives ONE round of the real dispatcher against the
// observing tool and returns the results.
func dispatchObservingTool(t *testing.T, l *Loop, mt agenttool.Tool) []agenttool.Result {
	t.Helper()
	ops := operations.New(nil, nil)
	op := ops.Begin("scratch")
	sess := &agenttool.SessionContext{Namespace: "default", Name: "disp", Operations: ops}

	uses := []llm.ToolUseBlock{{
		ID:    "tu-1",
		Name:  mt.Name(),
		Input: json.RawMessage(`{"operation_id":"` + op.ID + `","_reason":"read the pull request","args":{}}`),
	}}
	results, _ := l.dispatchToolUses(memory.WithSystemApproval(context.Background(), "test"),
		uses, sess, 0, 0, nil, nil)
	require.Len(t, results, 1)
	return results
}

// TestDispatch_ObservedFactBindsTheSlotWithoutAFurtherTurn is the call-site
// proof. One dispatch round: the tool runs, its observes block records a fact
// about git_commit:ba03f5969a, and by the time the round returns the slot
// holds a grant for that instance — no second turn, no test calling the
// promotion by hand.
//
// Delete or comment out the l.promoteObservedSlots(ctx) call in
// dispatchToolUses and this test fails with an empty grant set while every
// other suite stays green. That is the whole reason it exists.
func TestDispatch_ObservedFactBindsTheSlotWithoutAFurtherTurn(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	w := &observedRelWriter{}
	mt := observedMCPTool(t, mem)
	l := observedBindingLoop(t, mem, w, []agenttool.Tool{mt})

	results := dispatchObservingTool(t, l, mt)
	require.False(t, results[0].IsError, "the observing tool must succeed; content=%q", results[0].Content)

	// The fact really was recorded by the tool, through the production path —
	// asserted first so a failure below is unambiguously the BINDING and not a
	// tool that never observed anything.
	facts, err := observedfact.ForSubject(memory.WithSystemApproval(context.Background(), "test"),
		mem, memory.Scope{Kind: "session", ID: "default/disp"}, "git_commit", "ba03f5969a")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"is_cross_repository": false}, facts,
		"the tool's observes block must have recorded the fact")

	require.Len(t, w.wrote, 1, "the recorded fact must have bound the slot within this same round")
	assert.Equal(t, "git_commit", w.wrote[0].ResourceType)
	assert.Equal(t, "ba03f5969a", w.wrote[0].ResourceID)
	assert.Equal(t, authz.SlotGrantRelationName("read"), w.wrote[0].Relation)
	assert.Equal(t, "default/disp", w.wrote[0].SubjectID, "the grant is scoped to this session")
}

// The same round, with the slot declaring `fillFrom: [default]` instead. The
// fact is still recorded and nothing binds — proof that the fill source Task 4
// registered governs this path in production, not only in pkg/authz's own
// table.
func TestDispatch_ObservedFactDoesNotBindASlotThatDoesNotAdmitIt(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	w := &observedRelWriter{}
	mt := observedMCPTool(t, mem)
	l := observedBindingLoop(t, mem, w, []agenttool.Tool{mt})
	l.AgentClass.Spec.Authz.Slots[0].FillFrom = []string{"default"}

	results := dispatchObservingTool(t, l, mt)
	require.False(t, results[0].IsError, "content=%q", results[0].Content)

	facts, err := observedfact.ForSubject(memory.WithSystemApproval(context.Background(), "test"),
		mem, memory.Scope{Kind: "session", ID: "default/disp"}, "git_commit", "ba03f5969a")
	require.NoError(t, err)
	require.NotEmpty(t, facts, "the fact is recorded either way; only the BINDING is gated")

	assert.Empty(t, w.wrote, "a slot the class pins its own ids for must not be filled by an observation")
}
