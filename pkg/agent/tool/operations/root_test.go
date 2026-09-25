package operations_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
)

// The session root is the anchor that makes the audit graph TOTAL: a call with
// no explicit operation and no in-progress plan item still attributes
// somewhere. Minted lazily so a session that never makes an unattributed call
// carries no phantom node.
func TestRoot_isMintedOnceAndIsStable(t *testing.T) {
	r := operations.New(func() time.Time { return time.Unix(0, 0) }, nil)

	first := r.Root()
	second := r.Root()

	assert.NotEmpty(t, first.ID, "the root must be a real, addressable operation")
	assert.Equal(t, first.ID, second.ID,
		"a second Root must return the SAME operation; a per-call root would scatter "+
			"the audit graph into one node per unattributed call")

	assert.Len(t, r.All(), 1, "Root is idempotent — it must not mint on every read")
}

// Root must be reachable through the ordinary Get/RecordCall path, or the
// ambient resolver cannot use it: it hands the resolved ID to the same
// recording code every other call goes through.
func TestRoot_recordsCallsLikeAnyOtherOperation(t *testing.T) {
	r := operations.New(nil, nil)
	root := r.Root()

	got, ok := r.Get(root.ID)
	require.True(t, ok, "the root must be Get-able by its own ID")
	assert.True(t, got.Root, "the root must identify itself; the activity tree filters on this")

	idx, ok := r.RecordCall(root.ID, tool.OperationCall{Tool: "query_memory", Reason: "recall"})
	require.True(t, ok)
	assert.True(t, r.CompleteCall(root.ID, idx))
}

// An ordinary operation must NOT claim to be the root — the activity tree
// filters on this flag, so a false positive would hide real work.
func TestBegin_doesNotMarkOrdinaryOperationsAsRoot(t *testing.T) {
	r := operations.New(nil, nil)
	op := r.Begin("some work")
	assert.False(t, op.Root, "only Root() mints the root")
}
