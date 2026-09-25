package relwrites

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_UnmarkedBlockNeverConsultsChecker proves RequireSlotBound == false
// is byte-identical to today's behaviour: the checker is never called, and
// the tuple is written regardless of what the checker would have said.
func TestRun_UnmarkedBlockNeverConsultsChecker(t *testing.T) {
	blocks := []Block{
		{Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`}},
	}
	w := &fakeWriter{}
	calls := 0
	checker := SlotBoundChecker(func(_ context.Context, _ string) (bool, error) {
		calls++
		return false, nil // would refuse if consulted
	})
	written, err := Run(context.Background(), w, blocks, nil, checker, func(string, ...any) {})
	require.NoError(t, err)
	assert.Equal(t, 0, calls, "checker must never be consulted for an unmarked block")
	assert.Len(t, written, 1, "unmarked block's tuple must still be written")
	assert.Len(t, w.got, 1)
}

// TestRun_MarkedBlockNilCheckerRefusesUnwired proves a marked block whose
// checker is nil refuses every tuple of that block loudly, naming the block
// and saying the checker is unwired — never silently skipped.
func TestRun_MarkedBlockNilCheckerRefusesUnwired(t *testing.T) {
	blocks := []Block{
		{RequireSlotBound: true, Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`}},
	}
	w := &fakeWriter{}
	written, err := Run(context.Background(), w, blocks, nil, nil, func(string, ...any) {})
	require.Error(t, err, "a marked block with no checker must refuse, not succeed silently")
	assert.Contains(t, err.Error(), "block 0")
	assert.Contains(t, err.Error(), "unwired")
	assert.Empty(t, written, "nothing may be written from a block that could not be checked")
	assert.Empty(t, w.got, "writer must not be called for a refused block")
}

// TestRun_MarkedBlockCheckerFalseRefusesNamingResource proves a checker
// returning false refuses that specific tuple, naming its resource.
func TestRun_MarkedBlockCheckerFalseRefusesNamingResource(t *testing.T) {
	blocks := []Block{
		{RequireSlotBound: true, Tuple: Tuple{Resource: `"pull_request:42"`, Relation: `"r"`, Subject: `"b:1"`}},
	}
	w := &fakeWriter{}
	checker := SlotBoundChecker(func(_ context.Context, resource string) (bool, error) {
		assert.Equal(t, "pull_request:42", resource)
		return false, nil
	})
	written, err := Run(context.Background(), w, blocks, nil, checker, func(string, ...any) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull_request:42", "refusal must name the resource")
	assert.Empty(t, written)
	assert.Empty(t, w.got, "the refused tuple must not reach the writer")
}

// TestRun_MarkedBlockCheckerErrorRefusesNotSkips proves a checker error is a
// refusal, never a silent skip: an unreadable grant set is unknown, not
// empty, so it must surface as an error naming the resource.
func TestRun_MarkedBlockCheckerErrorRefusesNotSkips(t *testing.T) {
	blocks := []Block{
		{RequireSlotBound: true, Tuple: Tuple{Resource: `"pull_request:42"`, Relation: `"r"`, Subject: `"b:1"`}},
	}
	w := &fakeWriter{}
	checker := SlotBoundChecker(func(_ context.Context, _ string) (bool, error) {
		return false, fmt.Errorf("spicedb unavailable")
	})
	written, err := Run(context.Background(), w, blocks, nil, checker, func(string, ...any) {})
	require.Error(t, err, "a checker error must refuse, not be swallowed as a skip")
	assert.Contains(t, err.Error(), "pull_request:42")
	assert.Contains(t, err.Error(), "spicedb unavailable")
	assert.Empty(t, written)
	assert.Empty(t, w.got)
}

// TestRun_MarkedBlockChecker_PartialApprovalWithinForEach proves the check is
// genuinely per-tuple: a forEach block resolving two tuples, one approved and
// one refused by the checker, must write EXACTLY the approved tuple and the
// returned error must name EXACTLY the refused resource — the sibling tuple's
// fate must not follow the refused one's (over-blocking) nor vice versa
// (fail-open).
func TestRun_MarkedBlockChecker_PartialApprovalWithinForEach(t *testing.T) {
	blocks := []Block{
		{
			RequireSlotBound: true,
			ForEach:          "result.prs",
			Tuple: Tuple{
				Resource: `"pull_request:" + item.id`,
				Relation: `"r"`,
				Subject:  `"b:1"`,
			},
		},
	}
	vars := map[string]any{
		"result": map[string]any{
			"prs": []any{
				map[string]any{"id": "1"}, // approved
				map[string]any{"id": "2"}, // refused
			},
		},
	}
	w := &fakeWriter{}
	checker := SlotBoundChecker(func(_ context.Context, resource string) (bool, error) {
		return resource == "pull_request:1", nil
	})
	written, err := Run(context.Background(), w, blocks, vars, checker, func(string, ...any) {})

	require.Error(t, err, "the block must still report the one refusal")
	assert.Contains(t, err.Error(), "pull_request:2", "error must name exactly the refused resource")
	assert.NotContains(t, err.Error(), "pull_request:1", "the approved resource must not be named as refused")

	require.Len(t, written, 1, "exactly the approved tuple must be written — not zero (over-blocking), not both (fail-open)")
	assert.Equal(t, "pull_request:1", written[0].Resource)

	require.Len(t, w.got, 1, "writer must be called once, with only the approved tuple in its batch")
	require.Len(t, w.got[0], 1)
	assert.Equal(t, "pull_request:1", w.got[0][0].Resource)
}
