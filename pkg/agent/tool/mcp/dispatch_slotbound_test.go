package mcp_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/relwritesaudit"
)

// slotGrantListerStub stands in for (*spicedb.Client) so these dispatch-level
// tests drive the REAL relwrites.NewSlotBoundChecker and the REAL
// relwrites.Run — only the SpiceDB read is faked. A reimplementation of the
// gate here would pass whether or not the dispatcher actually consults it.
type slotGrantListerStub struct{ grants []authz.SlotBinding }

func (s slotGrantListerStub) ListSlotGrants(_ context.Context, _, _ string) ([]authz.SlotBinding, error) {
	return s.grants, nil
}

// markedPRWriteBlock is the CRD-form block under test: a write onto the
// pull_request the response named, marked requireSlotBound.
func markedPRWriteBlock() spiceboxv1alpha1.MCPServerRelationshipWrite {
	return spiceboxv1alpha1.MCPServerRelationshipWrite{
		When:             "has(result.results)",
		ForEach:          "result.results",
		RequireSlotBound: true,
		Tuple: spiceboxv1alpha1.MCPServerRelationshipTuple{
			Resource: `"pull_request:" + item.id`,
			Relation: `"author"`,
			Subject:  `"pr_author:" + item.author`,
		},
	}
}

const prWritePayload = `{"results":[{"id":"owner-repo-7","author":"dana"}]}`

// TestExecute_SlotBoundBlockWritesWhenTheGrantIsHeld proves the MCP
// conversion site carries requireSlotBound from the CRD into relwrites.Block
// AND that the wired checker's approval lets the tuple through. The grant is
// seeded in the lister the production checker reads.
func TestExecute_SlotBoundBlockWritesWhenTheGrantIsHeld(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prWritePayload, false)
	mt := mustSynthesize(t, srv.URL)[0].(*mcp.MCPTool)

	fw := &fakeRelWriter{}
	mt.SetRelWriter(fw)
	mt.SetWritesRelationships([]spiceboxv1alpha1.MCPServerRelationshipWrite{markedPRWriteBlock()})
	mt.SetSlotBoundChecker(relwrites.NewSlotBoundChecker(slotGrantListerStub{grants: []authz.SlotBinding{
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-7"), Permission: "read"},
	}}, "default", "sess"))

	_, opID, sess := newOpAndSess(t)
	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError; content=%q", res.Content)

	require.Len(t, fw.batches, 1, "the granted tuple must reach the writer")
	require.Len(t, fw.batches[0], 1)
	assert.Equal(t, "pull_request:owner-repo-7", fw.batches[0][0].Resource)
}

// TestExecute_SlotBoundBlockIsRefusedWithoutTheGrant is the same dispatch with
// the grant absent: nothing reaches the writer, and the refusal surfaces on
// the tool result where the agent sees it.
//
// Asserting on the WRITER (not only the result) is what makes this a gate
// test: a block that failed for any other reason would also flip IsError.
func TestExecute_SlotBoundBlockIsRefusedWithoutTheGrant(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prWritePayload, false)
	mt := mustSynthesize(t, srv.URL)[0].(*mcp.MCPTool)

	fw := &fakeRelWriter{}
	mt.SetRelWriter(fw)
	mt.SetWritesRelationships([]spiceboxv1alpha1.MCPServerRelationshipWrite{markedPRWriteBlock()})
	mt.SetSlotBoundChecker(relwrites.NewSlotBoundChecker(slotGrantListerStub{grants: []authz.SlotBinding{
		// A grant on a DIFFERENT pull request: the session holds slots, just
		// not this one. "holds nothing" would also pass a gate that only
		// checked for emptiness.
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-8"), Permission: "read"},
	}}, "default", "sess"))

	_, opID, sess := newOpAndSess(t)
	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute returns no transport error")
	assert.Empty(t, fw.batches, "an unbound resource's tuple must never reach the writer; got %+v", fw.batches)
	assert.True(t, res.IsError, "the refusal must surface on the tool result")
	assert.Contains(t, res.Content, "pull_request:owner-repo-7", "the refusal must name the resource")
	assert.Contains(t, res.Content, "no slot-bound grant")
}

// TestExecute_UnmarkedBlockStillWritesWithoutAnyGrant pins the default: a
// block that does NOT declare requireSlotBound is byte-identical to today —
// it writes with no grant and no checker consulted. Without this row, wiring
// the flag on unconditionally would still pass the two rows above.
func TestExecute_UnmarkedBlockStillWritesWithoutAnyGrant(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prWritePayload, false)
	mt := mustSynthesize(t, srv.URL)[0].(*mcp.MCPTool)

	block := markedPRWriteBlock()
	block.RequireSlotBound = false

	fw := &fakeRelWriter{}
	mt.SetRelWriter(fw)
	mt.SetWritesRelationships([]spiceboxv1alpha1.MCPServerRelationshipWrite{block})
	mt.SetSlotBoundChecker(relwrites.NewSlotBoundChecker(slotGrantListerStub{}, "default", "sess"))

	_, opID, sess := newOpAndSess(t)
	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")
	require.False(t, res.IsError, "unexpected IsError; content=%q", res.Content)
	require.Len(t, fw.batches, 1, "an unmarked block must write with no grant at all")
	assert.Equal(t, "pull_request:owner-repo-7", fw.batches[0][0].Resource)
}

// TestExecute_AuditRecordsTuplesThatLandedDespiteARefusedSibling proves the
// relwrites_audit record survives a partial refusal.
//
// relwrites.Run is partial by construction — an earlier block writes, a later
// marked one is refused, and both outcomes come back from the same call. Before
// requireSlotBound, partial refusal needed a CEL bug or a SpiceDB rejection to
// happen at all; the per-tuple gate makes it a ROUTINE outcome. An audit trail
// that goes quiet exactly when the gate acts is the wrong direction: the
// tamper-evident answer to "what tuples did this session generate?" must still
// name the tuples that really landed in SpiceDB, refusal or no refusal.
func TestExecute_AuditRecordsTuplesThatLandedDespiteARefusedSibling(t *testing.T) {
	srv, _ := newTextToolServer(t, "search_issues", prWritePayload, false)
	mt := mustSynthesize(t, srv.URL)[0].(*mcp.MCPTool)

	// Block 0 is unmarked and writes. Block 1 is marked against a resource the
	// session holds no grant on, so it is refused — and Run returns BOTH.
	landed := markedPRWriteBlock()
	landed.RequireSlotBound = false
	refused := markedPRWriteBlock()
	refused.Tuple.Resource = `"pull_request:owner-repo-9"`

	fw := &fakeRelWriter{}
	mt.SetRelWriter(fw)
	mt.SetWritesRelationships([]spiceboxv1alpha1.MCPServerRelationshipWrite{landed, refused})
	mt.SetSlotBoundChecker(relwrites.NewSlotBoundChecker(slotGrantListerStub{}, "default", "sess"))
	mem, scope := newObserveMemory(t, mt)

	_, opID, sess := newOpAndSess(t)
	res, err := mt.Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")

	// The refusal still aborts the call — that half is unchanged.
	require.True(t, res.IsError, "the refusal must still surface")
	require.Contains(t, res.Content, "pull_request:owner-repo-9")

	// …and block 0's tuple really reached SpiceDB.
	require.Len(t, fw.batches, 1, "the unmarked block's tuple must have been written")
	require.Equal(t, "pull_request:owner-repo-7", fw.batches[0][0].Resource)

	// The claim under test: that written tuple is in the audit.
	audits, aerr := relwritesaudit.ByToolCall(readCtx(), mem, scope, "")
	require.NoError(t, aerr, "reading relwrites_audit back")
	var recorded []string
	for _, a := range audits {
		for _, tu := range a.Tuples {
			recorded = append(recorded, tu.Resource)
		}
	}
	assert.Contains(t, recorded, "pull_request:owner-repo-7",
		"a tuple that WAS written must be audited even though a sibling block was refused")
	assert.NotContains(t, recorded, "pull_request:owner-repo-9",
		"a refused tuple was never written and must not be audited as though it were")
}
