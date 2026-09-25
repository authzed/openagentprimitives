package runner

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func TestBuildOperationActivity(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	cases := []struct {
		name        string
		ops         []tool.Operation
		wantActive  bool
		wantOps     int
		wantCompact string
	}{
		{
			name:       "fresh op under gate: nothing active",
			ops:        []tool.Operation{{ID: "op1", Description: "d", CreatedAt: now.Add(-1 * time.Second)}},
			wantActive: false,
		},
		{
			name: "op past gate with active call: included + compact line",
			ops: []tool.Operation{{
				ID: "op1", Description: "fetch goals", CreatedAt: now.Add(-5 * time.Second),
				Calls: []tool.OperationCall{{Tool: "linear", Reason: "querying", At: now.Add(-3 * time.Second)}},
			}},
			wantActive:  true,
			wantOps:     1,
			wantCompact: "fetch goals ‣ querying",
		},
		{
			name:       "no ops: not active",
			ops:        nil,
			wantActive: false,
		},
		{
			name: "closed op: excluded even though past the activation gate",
			ops: []tool.Operation{{
				ID: "op1", Description: "done work", CreatedAt: now.Add(-10 * time.Second), Closed: true,
			}},
			wantActive: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl, active := buildOperationActivity(tc.ops, now, act)
			assert.Equal(t, tc.wantActive, active)
			if tc.wantActive {
				require.Len(t, pl.Operations, tc.wantOps)
				assert.Equal(t, tc.wantCompact, pl.CompactLine)
			}
		})
	}
}

// TestBuildOperationActivity_NestedOrderingAndCompactLine verifies the
// ordering invariant (a parent operation precedes its children in the
// returned slice, regardless of input order) and that the compact line
// resolves to the deepest active operation's active call, per
// channelevents.OperationActivityCompactLine's "last active op wins"
// heuristic.
func TestBuildOperationActivity_NestedOrderingAndCompactLine(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	ops := []tool.Operation{
		// Deliberately listed deep-first to prove the builder re-sorts by
		// depth rather than passing input order through untouched.
		{
			ID: "op-child", Description: "inner", CreatedAt: now.Add(-4 * time.Second),
			Parent: &tool.OperationParent{OperationID: "op-parent"},
			// op-child is 4s old (past the 2s gate on its own CreatedAt) and has
			// an in-flight call, so it is included; its active call's reason
			// drives the compact line.
			Calls: []tool.OperationCall{{Tool: "linear", Reason: "querying now", At: now.Add(-3 * time.Second)}},
		},
		{
			ID: "op-parent", Description: "outer", CreatedAt: now.Add(-9 * time.Second),
		},
	}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active)
	require.Len(t, pl.Operations, 2)
	assert.Equal(t, "op-parent", pl.Operations[0].ID, "parent op must precede its child")
	assert.Equal(t, "op-child", pl.Operations[1].ID)
	assert.Equal(t, "op-parent", pl.Operations[1].ParentOpID)
	assert.Equal(t, "inner ‣ querying now", pl.CompactLine, "deepest active op with its active call wins")
}

// TestBuildOperationActivity_TruncatesInFlightCallsToNewestEight covers the
// per-operation call-count bound: only the newest maxOperationActivityCalls
// (8) in-flight calls are kept, and every surfaced call is marked Active
// (completed calls never make it into node.Calls at all — see the
// dedicated drop test below).
func TestBuildOperationActivity_TruncatesInFlightCallsToNewestEight(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	var calls []tool.OperationCall
	for i := 0; i < 10; i++ {
		calls = append(calls, tool.OperationCall{
			Tool: "t", Reason: fmt.Sprintf("call-%d", i), At: now.Add(-time.Duration(10-i) * time.Second),
		})
	}
	ops := []tool.Operation{{ID: "op1", Description: "d", CreatedAt: now.Add(-20 * time.Second), Calls: calls}}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active)
	require.Len(t, pl.Operations, 1)
	require.Len(t, pl.Operations[0].Calls, 8, "kept only the newest 8 of 10 in-flight calls")
	assert.Equal(t, "call-2", pl.Operations[0].Calls[0].Reason, "oldest 2 calls dropped")
	assert.Equal(t, "call-9", pl.Operations[0].Calls[7].Reason, "newest call retained")
	assert.True(t, pl.Operations[0].Calls[0].Active, "every surfaced in-flight call is Active")
	assert.True(t, pl.Operations[0].Calls[7].Active, "every surfaced in-flight call is Active")
}

// TestBuildOperationActivity_OrphanAllCallsCompletedExcluded covers the
// core of the fix: an operation is never Closed in normal flow, but once
// every call it has recorded has returned, it has no "current work" left
// and must stop appearing in the live tree — otherwise every op ever
// opened accumulates forever.
func TestBuildOperationActivity_OrphanAllCallsCompletedExcluded(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	ops := []tool.Operation{{
		ID: "op1", Description: "long since done", CreatedAt: now.Add(-30 * time.Second),
		Calls: []tool.OperationCall{
			{Tool: "t", Reason: "r", At: now.Add(-20 * time.Second), CompletedAt: now.Add(-19 * time.Second)},
		},
	}}

	pl, active := buildOperationActivity(ops, now, act)
	assert.False(t, active, "an op whose only call already completed is an orphan and must be excluded")
	assert.Empty(t, pl.Operations)
}

// TestBuildOperationActivity_CompletedCallsDroppedFromNode covers the case
// where an operation has a mix of completed and in-flight calls: it stays
// included (still has current work) but only the in-flight call surfaces
// in node.Calls.
func TestBuildOperationActivity_CompletedCallsDroppedFromNode(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	ops := []tool.Operation{{
		ID: "op1", Description: "d", CreatedAt: now.Add(-10 * time.Second),
		Calls: []tool.OperationCall{
			{Tool: "old", Reason: "done already", At: now.Add(-8 * time.Second), CompletedAt: now.Add(-7 * time.Second)},
			{Tool: "cur", Reason: "still going", At: now.Add(-3 * time.Second)},
		},
	}}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active)
	require.Len(t, pl.Operations, 1)
	require.Len(t, pl.Operations[0].Calls, 1, "completed call dropped; only the in-flight call surfaces")
	assert.Equal(t, "still going", pl.Operations[0].Calls[0].Reason)
	assert.True(t, pl.Operations[0].Calls[0].Active)
}

// TestBuildOperationActivity_InFlightGatedByOpAge_NoBlinkBetweenCalls covers
// the between-calls blink fix: an operation with current work is gated on the
// OPERATION's age (CreatedAt), not the individual in-flight call's age. A
// continuously-busy op that has already passed the gate must stay surfaced when
// one call completes and the next is freshly dispatched — even though that new
// in-flight call is younger than the activation window — instead of blinking
// out for `activation` in the gap between calls.
func TestBuildOperationActivity_InFlightGatedByOpAge_NoBlinkBetweenCalls(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	ops := []tool.Operation{{
		ID: "op1", Description: "fetch data", CreatedAt: now.Add(-5 * time.Second), // op is well past the gate
		Calls: []tool.OperationCall{
			// a prior call already completed…
			{Tool: "linear", Reason: "done", At: now.Add(-4 * time.Second), CompletedAt: now.Add(-2500 * time.Millisecond)},
			// …and the next was just dispatched (younger than the gate).
			{Tool: "linear", Reason: "querying", At: now.Add(-500 * time.Millisecond)},
		},
	}}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active, "a busy op past the gate must not blink out just because its newest call is fresh")
	require.Len(t, pl.Operations, 1)
	require.Len(t, pl.Operations[0].Calls, 1, "only the in-flight call surfaces")
	assert.Equal(t, "querying", pl.Operations[0].Calls[0].Reason)
	assert.True(t, pl.Operations[0].Calls[0].Active)
}

// TestBuildOperationActivity_NoCallsYetGatedByCreatedAt covers the other
// half of the gate: an operation with no calls at all (just opened via
// new_operation, no dispatch yet) is gated on its own CreatedAt.
func TestBuildOperationActivity_NoCallsYetGatedByCreatedAt(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	cases := []struct {
		name       string
		createdAt  time.Time
		wantActive bool
	}{
		{"under gate: excluded", now.Add(-1 * time.Second), false},
		{"past gate: included", now.Add(-3 * time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := []tool.Operation{{ID: "op1", Description: "d", CreatedAt: tc.createdAt}}
			pl, active := buildOperationActivity(ops, now, act)
			assert.Equal(t, tc.wantActive, active)
			if tc.wantActive {
				require.Len(t, pl.Operations, 1)
				assert.Empty(t, pl.Operations[0].Calls, "no calls yet -> no call nodes")
			}
		})
	}
}

// TestBuildOperationActivity_EqualDepthSortedByCreatedAt covers the
// deterministic tie-break: Registry.All() returns map order (nondeterministic
// across ticks), so two equal-depth operations must sort by CreatedAt
// ascending rather than passing input order through — otherwise the compact
// line (which picks the LAST node) would flicker between the two on every
// heartbeat tick.
func TestBuildOperationActivity_EqualDepthSortedByCreatedAt(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	// Deliberately input the newer op first to prove the sort key is
	// CreatedAt, not input order.
	ops := []tool.Operation{
		{ID: "op-b", Description: "b", CreatedAt: now.Add(-5 * time.Second)},
		{ID: "op-a", Description: "a", CreatedAt: now.Add(-9 * time.Second)},
	}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active)
	require.Len(t, pl.Operations, 2)
	assert.Equal(t, "op-a", pl.Operations[0].ID, "older op sorts first at equal depth")
	assert.Equal(t, "op-b", pl.Operations[1].ID)
}

// TestBuildOperationActivity_DropsOperationsPastMaxDepth covers the tree-depth
// bound: a chain deeper than maxOperationActivityDepth (4) is truncated,
// dropping the operation(s) past the bound rather than surfacing an
// unbounded tree.
func TestBuildOperationActivity_DropsOperationsPastMaxDepth(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	ops := []tool.Operation{
		{ID: "op-0", Description: "d0", CreatedAt: now.Add(-20 * time.Second)},
		{ID: "op-1", Description: "d1", CreatedAt: now.Add(-20 * time.Second), Parent: &tool.OperationParent{OperationID: "op-0"}},
		{ID: "op-2", Description: "d2", CreatedAt: now.Add(-20 * time.Second), Parent: &tool.OperationParent{OperationID: "op-1"}},
		{ID: "op-3", Description: "d3", CreatedAt: now.Add(-20 * time.Second), Parent: &tool.OperationParent{OperationID: "op-2"}},
		{ID: "op-4", Description: "d4", CreatedAt: now.Add(-20 * time.Second), Parent: &tool.OperationParent{OperationID: "op-3"}},
		{ID: "op-5", Description: "d5 (too deep)", CreatedAt: now.Add(-20 * time.Second), Parent: &tool.OperationParent{OperationID: "op-4"}},
	}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active)
	ids := make([]string, len(pl.Operations))
	for i, n := range pl.Operations {
		ids[i] = n.ID
	}
	assert.NotContains(t, ids, "op-5", "depth-5 operation must be dropped")
	assert.Len(t, pl.Operations, 5, "op-0..op-4 (depths 0..4) are kept")
}

// TestBuildOperationActivity_PlanItemParent covers the plan-item-anchored
// parent path: PlanItem correlation is passed through (by item ID) rather
// than the sibling-operation ParentOpID field.
func TestBuildOperationActivity_PlanItemParent(t *testing.T) {
	now := time.Unix(1000, 0)
	act := 2 * time.Second
	ops := []tool.Operation{{
		ID: "op1", Description: "d", CreatedAt: now.Add(-5 * time.Second),
		Parent: &tool.OperationParent{PlanItem: &tool.PlanItemRef{Plan: "plan-a", Item: "item-1"}},
	}}

	pl, active := buildOperationActivity(ops, now, act)
	require.True(t, active)
	require.Len(t, pl.Operations, 1)
	require.NotNil(t, pl.Operations[0].PlanItem)
	assert.Equal(t, "item-1", pl.Operations[0].PlanItem.ID)
	assert.Empty(t, pl.Operations[0].ParentOpID)
}

// The root anchors the audit graph; it is not work the agent declared. It must
// never surface in the live activity tree — and the reason it would is subtle:
// buildOperationActivity counts a call-less operation as HAVING current work
// (`len(op.Calls) == 0`), so a freshly-minted root qualifies on its own. Left
// unfiltered it sits in the tree for the whole session saying nothing, and its
// ambient calls would mix unattributed work in with the agent's own.
func TestBuildOperationActivity_excludesTheSessionRoot(t *testing.T) {
	now := time.Unix(1000, 0)
	root := tool.Operation{ID: "root", Description: "session", CreatedAt: now, Root: true}

	_, active := buildOperationActivity([]tool.Operation{root}, now, 0)
	assert.False(t, active,
		"a root alone must produce no snapshot; it would otherwise be a permanent, "+
			"contentless node for the life of the session")

	real := tool.Operation{
		ID: "op1", Description: "real work", CreatedAt: now,
		Calls: []tool.OperationCall{{Tool: "code_gh", At: now}},
	}
	payload, active := buildOperationActivity([]tool.Operation{root, real}, now, 0)
	require.True(t, active, "a real operation still surfaces alongside a root")
	for _, n := range payload.Operations {
		assert.NotEqual(t, "root", n.ID, "the root must be filtered out, not merely outranked")
	}
}
