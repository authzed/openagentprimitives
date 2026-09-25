package relwrites

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RequireSlotBound is per-block opt-in, and ValidateResolvedTuple admits
// github_user as a subject unconditionally — nothing ties the two together.
// Without this gate, a writesRelationships block naming a github_user
// SUBJECT on a tenant resource, written WITHOUT requireSlotBound: true, would
// pass both existing checks: ValidateResolvedTuple admits the type, and the
// slot-bound checker is only ever consulted for a block that opted in. These
// two tests prove the gate closes that gap: a github_user subject is refused
// from an unmarked block, and still writes through once the block is marked
// and its resource is approved.

// TestRun_GitHubUserSubjectRefusedWithoutSlotBoundBlock proves the
// subject-type gate fires independently of — and BEFORE — RequireSlotBound's
// own checker path: a github_user subject from an unmarked block is refused
// even when a checker is wired and would approve every resource it is asked
// about.
func TestRun_GitHubUserSubjectRefusedWithoutSlotBoundBlock(t *testing.T) {
	blocks := []Block{
		{Tuple: Tuple{
			Resource: `"github_pull_request:PR_kwABC"`,
			Relation: `"author"`,
			Subject:  `"github_user:U_kwXYZ"`,
		}},
	}
	w := &fakeWriter{}
	calls := 0
	checker := SlotBoundChecker(func(_ context.Context, _ string) (bool, error) {
		calls++
		return true, nil // would approve if consulted
	})
	written, err := Run(context.Background(), w, blocks, nil, checker, func(string, ...any) {})
	require.Error(t, err, "a github_user subject from a non-slot-bound block must be refused")
	assert.Contains(t, err.Error(), `tuple subject type "github_user" requires a slot-bound block`)
	assert.Contains(t, err.Error(), "requireSlotBound: true", "the refusal must name the fix")
	// This is an authorization ruling, not a mechanism failure: a caller must
	// be able to tell it apart from an ordinary CEL/store error the same way
	// it tells apart a filterSlotBound refusal, via the shared sentinel — see
	// ValidateSlotBoundSubject's doc comment for why the error is tagged.
	assert.True(t, errors.Is(err, ErrSlotBoundRefused),
		"the structural-tie refusal must carry the gate's sentinel so a dispatcher (e.g. the sandbox path's evaluateWritesRelationships) surfaces it instead of swallowing it as a mechanism failure: %v", err)
	assert.Equal(t, 0, calls, "the slot-bound checker must never be consulted — this gate refuses before that path runs")
	assert.Empty(t, written, "nothing may be written when the subject-type gate refuses")
	assert.Empty(t, w.got, "writer must not be called for a refused block")
}

// TestRun_GitHubUserSubjectWritesFromASlotBoundBlock proves the gate closes
// an UNMARKED block, not the type itself: the same subject, from a block
// that declares RequireSlotBound and whose resource the checker approves,
// writes through exactly as any other slot-bound tuple would.
func TestRun_GitHubUserSubjectWritesFromASlotBoundBlock(t *testing.T) {
	blocks := []Block{
		{
			RequireSlotBound: true,
			Tuple: Tuple{
				Resource: `"github_pull_request:PR_kwABC"`,
				Relation: `"author"`,
				Subject:  `"github_user:U_kwXYZ"`,
			},
		},
	}
	w := &fakeWriter{}
	checker := SlotBoundChecker(func(_ context.Context, resource string) (bool, error) {
		assert.Equal(t, "github_pull_request:PR_kwABC", resource)
		return true, nil
	})
	written, err := Run(context.Background(), w, blocks, nil, checker, func(string, ...any) {})
	require.NoError(t, err)
	require.Len(t, written, 1, "the slot-bound block's tuple must be written")
	assert.Equal(t, "github_user:U_kwXYZ", written[0].Subject)
	require.Len(t, w.got, 1)
	require.Len(t, w.got[0], 1)
}
