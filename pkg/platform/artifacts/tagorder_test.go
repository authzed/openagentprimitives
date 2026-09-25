package artifacts_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// The tag list a revision carries is derived by scanning head.Tags, a Go map,
// and Go randomizes map iteration. That list is not internal: it is
// RevisionResult.Tags (artifact_prepare / artifact_await) and RevisionView.Tags
// (artifact_history) — bytes the MODEL reads. Before the sort, two identical
// finalizes of one revision returned the same set in a different sequence.
//
// A single call cannot see that, which is exactly why it survived review and a
// green suite: it took replaying a captured session and diffing the result
// against its own recording to notice. These tests repeat the operation instead,
// which is the same evidence available in a unit test.
//
// tagOrderTrials is chosen so a two-element map that came out in random order
// would slip through with probability 2^-(n-1) ~ 2e-6 rather than the 1/2 a
// single run gives. The whole file costs a few milliseconds.
const tagOrderTrials = 20

// finalizeTagged finalizes one revision carrying the given comma-separated
// applied tags and returns the tag list the service reports for it.
func finalizeTagged(t *testing.T, appliedTags string) []string {
	t.Helper()
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	cr := renderedCR("ar-1", headID, "", "initial draft", appliedTags, types.UID("uid-1"))
	rev, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize")
	return rev.Tags
}

func TestFinalizeRevision_TagsAreInAscendingNameOrder(t *testing.T) {
	// "latest" is applied by the service on top of whatever the caller asked
	// for, so this revision carries four tags whose alphabetical order differs
	// from both their applied order and any latest-first rule.
	got := finalizeTagged(t, "b0f3d1c2,approved,zeta")

	assert.Equal(t, []string{"approved", "b0f3d1c2", "latest", "zeta"}, got,
		"tags come out in ascending name order — see artifacts.tagsPointingAt for "+
			"why the order is arbitrary-but-stable rather than latest-first")
}

func TestFinalizeRevision_TagOrderIsStableAcrossRuns(t *testing.T) {
	// The property the assertion above cannot state on its own: repeating the
	// identical operation must yield the identical sequence. This is the check
	// that fails if the sort is removed, no matter which order is chosen.
	first := finalizeTagged(t, "b0f3d1c2,approved,zeta")
	require.Len(t, first, 4, "the fixture must actually produce a multi-tag list, "+
		"or this test would pass while asserting nothing")

	for i := 1; i < tagOrderTrials; i++ {
		got := finalizeTagged(t, "b0f3d1c2,approved,zeta")
		require.Equal(t, first, got,
			"run %d returned the same tags in a different order; the list is "+
				"handed to the model, so two identical operations must encode identically", i)
	}
}

func TestRevisionTree_TagOrderIsStableAcrossRuns(t *testing.T) {
	// artifact_history reads RevisionView.Tags from the same derivation, so it
	// is covered by the same fix — but through a different call, and a fix
	// applied at one caller instead of at the source would pass the test above
	// and fail this one.
	svc, scope := newSvc()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	headID := svc.NewArtifactID()

	cr := renderedCR("ar-1", headID, "", "initial draft", "b0f3d1c2,approved,zeta", types.UID("uid-1"))
	_, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize")

	var first []string
	for i := range tagOrderTrials {
		views, err := svc.RevisionTree(ctx, scope, headID)
		require.NoError(t, err, "list revisions")
		require.Len(t, views, 1, "one revision was finalized")
		if i == 0 {
			first = views[0].Tags
			require.Len(t, first, 4, "the fixture must actually produce a multi-tag list")
			continue
		}
		require.Equal(t, first, views[0].Tags,
			"listing %d returned the same tags in a different order", i)
	}
	assert.Equal(t, []string{"approved", "b0f3d1c2", "latest", "zeta"}, first,
		"and in the same ascending order FinalizeRevision reports")
}
