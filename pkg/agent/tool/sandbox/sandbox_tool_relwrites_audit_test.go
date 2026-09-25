package sandbox

// Dispatch-level tests proving the sandbox path records a relwrites_audit
// entry for every tuple that actually lands in SpiceDB — mirroring
// pkg/agent/tool/mcp/dispatch_slotbound_test.go's
// TestExecute_AuditRecordsTuplesThatLandedDespiteARefusedSibling. Before this
// file's fix, evaluateWritesRelationships discarded relwrites.Run's `written`
// return entirely: the sandbox path is the one the shipped reviewbot demo's
// PR-identity tuples actually use (SpiceboxToolspec, dispatched by a
// sandboxed tool call — an MCP toolspec's writesRelationships block would
// dispatch through the MCP path this file's sibling covers instead), and its
// writes reached SpiceDB with no audit trail at all. Beyond visibility, that
// silently corrupted a steelthread capture: pkg/steelthread/read.go derives
// its SpiceDB seed by subtracting AUDITED tuples, so an unaudited
// sandbox-written tuple folds into a captured seed as though a human had put
// it there.
//
// package sandbox (internal), calling evaluateWritesRelationships directly —
// same convention as sandbox_tool_slotbound_test.go.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// unmarkedPRWriteSpec is a plain (non-slot-bound) write, so these tests
// exercise the audit trail independent of the slot-bound gate under test
// elsewhere in this package.
func unmarkedPRWriteSpec() spec.RelationshipWriteSpec {
	return spec.RelationshipWriteSpec{
		When: "result.success",
		Tuple: spec.RelationshipTupleSpec{
			Resource: `"pull_request:" + args.argv[1]`,
			Relation: `"observed_by"`,
			Subject:  `"agentsession:" + session`,
		},
	}
}

// Both tests below wire the SandboxTool with a REAL in-memory facade
// (memory.NewLocal(inmem.NewBackend())) via SetMemory — the same setter
// internal/cmd/runner/main.go calls in production alongside
// SetRelWriter/SetSlotBoundChecker — so relwritesaudit.ByToolCall reads back
// a real append-only Put, not a stand-in for one. recordingRelWriter and
// newSandboxToolForRelwrites/newSlotBoundSandboxTool/markedPRWriteSpec are
// defined in the neighbouring sandbox_tool_relwrites_stdout_test.go and
// sandbox_tool_slotbound_test.go.

func TestSandboxRelwrites_AuditRecordsAWrittenTuple(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{unmarkedPRWriteSpec()},
	})
	mem := memory.NewLocal(inmem.NewBackend())
	st.SetMemory(mem)
	scope := memory.Scope{Kind: "session", ID: "default/sess"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	st.evaluateWritesRelationships(ctx,
		tool.Result{}, "", []string{"view", "owner-repo-7"}, testSession(t))

	require.Len(t, rec.writes, 1, "the unmarked block must have written")

	audits, err := relwritesaudit.ByToolCall(ctx, mem, scope, "")
	require.NoError(t, err, "reading relwrites_audit back")
	var recorded []string
	for _, a := range audits {
		for _, tu := range a.Tuples {
			recorded = append(recorded, tu.Resource)
		}
	}
	assert.Contains(t, recorded, "pull_request:owner-repo-7",
		"a tuple the sandbox path wrote to SpiceDB must be audited — steelthread's seed derivation "+
			"(pkg/steelthread/read.go) subtracts audited tuples, so an unaudited write would be "+
			"captured into a seed as though a human had put it there")
}

// TestSandboxRelwrites_RefusedCallRecordsNoAudit is the negative: a call
// that wrote NOTHING (every tuple refused by the slot-bound gate) must file
// no entry at all — an empty entry costs a link in the append-only chain and
// answers nothing, the same property relwritesaudit.RecordWritten's own doc
// comment states.
func TestSandboxRelwrites_RefusedCallRecordsNoAudit(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSlotBoundSandboxTool(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{markedPRWriteSpec()},
	}, nil) // no grant: the marked block is refused
	mem := memory.NewLocal(inmem.NewBackend())
	st.SetMemory(mem)
	scope := memory.Scope{Kind: "session", ID: "default/sess"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	got := st.evaluateWritesRelationships(ctx,
		tool.Result{Content: "the pr view output"}, "", []string{"view", "owner-repo-7"}, testSession(t))

	require.True(t, got.IsError, "sanity: the slot-bound refusal must still surface")
	assert.Empty(t, rec.writes, "nothing may have reached the writer")

	audits, err := relwritesaudit.ByToolCall(ctx, mem, scope, "")
	require.NoError(t, err, "reading relwrites_audit back")
	assert.Empty(t, audits, "a call that wrote nothing must not file an empty audit entry")
}
