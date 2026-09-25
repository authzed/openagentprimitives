package guardian_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
)

func tupAlice() spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: "group", ResourceID: "eng",
		Relation:    "member",
		SubjectType: "user", SubjectID: "alice-canon",
	}
}
func tupBob() spicedb.Tuple {
	return spicedb.Tuple{
		ResourceType: "team", ResourceID: "alpha",
		Relation:    "lead",
		SubjectType: "user", SubjectID: "bob-canon",
	}
}

func ownerSet(owners ...guardian.Owner) map[string]guardian.Owner {
	out := map[string]guardian.Owner{}
	for _, o := range owners {
		out[o.CR] = o
	}
	return out
}

func TestDiff_AddedTuple_ToTouch(t *testing.T) {
	last := guardian.NewDesiredMap()
	new := guardian.NewDesiredMap()
	new.Add(tupAlice(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})

	diff := guardian.ComputeDiff(last, new)
	require.Len(t, diff.ToTouch, 1)
	assert.Equal(t, tupAlice().Key(), diff.ToTouch[0].Key())
	assert.Empty(t, diff.ToDelete)
}

func TestDiff_DroppedTuple_SoleDeleteOwner_ToDelete(t *testing.T) {
	last := guardian.NewDesiredMap()
	last.Add(tupAlice(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})
	new := guardian.NewDesiredMap()

	diff := guardian.ComputeDiff(last, new)
	require.Len(t, diff.ToDelete, 1)
	assert.Equal(t, tupAlice().Key(), diff.ToDelete[0].Key())
}

func TestDiff_DroppedTuple_AllRetain_NotDeleted(t *testing.T) {
	last := guardian.NewDesiredMap()
	last.Add(tupAlice(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimRetain})
	last.Add(tupAlice(), guardian.Owner{CR: "cr2", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimRetain})
	new := guardian.NewDesiredMap()

	diff := guardian.ComputeDiff(last, new)
	assert.Empty(t, diff.ToDelete, "no Delete owners → no delete")
}

func TestDiff_DroppedTuple_MixedOwners_DeleteByDeleteOwner(t *testing.T) {
	// One Delete owner, one Retain owner; tuple disappears from BOTH.
	// Delete fires because some Delete owner once claimed it.
	last := guardian.NewDesiredMap()
	last.Add(tupAlice(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})
	last.Add(tupAlice(), guardian.Owner{CR: "cr2", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimRetain})
	new := guardian.NewDesiredMap()

	diff := guardian.ComputeDiff(last, new)
	require.Len(t, diff.ToDelete, 1)
}

func TestDiff_StillClaimed_NotDeleted(t *testing.T) {
	// cr1 drops, cr2 still claims (also Delete).
	last := guardian.NewDesiredMap()
	last.Add(tupAlice(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})
	last.Add(tupAlice(), guardian.Owner{CR: "cr2", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})

	new := guardian.NewDesiredMap()
	new.Add(tupAlice(), guardian.Owner{CR: "cr2", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})

	diff := guardian.ComputeDiff(last, new)
	assert.Empty(t, diff.ToDelete)
	require.Len(t, diff.ToTouch, 1)
}

func TestDiff_DeterministicOrder(t *testing.T) {
	last := guardian.NewDesiredMap()
	new := guardian.NewDesiredMap()
	// Insert bob first, alice second; expect Tuple.Key() ascending in output.
	new.Add(tupBob(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})
	new.Add(tupAlice(), guardian.Owner{CR: "cr1", Policy: spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete})

	diff := guardian.ComputeDiff(last, new)
	require.Len(t, diff.ToTouch, 2)
	assert.Less(t, diff.ToTouch[0].Key(), diff.ToTouch[1].Key(), "ToTouch must be sorted by Tuple.Key()")
	// Call-to-call stability.
	again := guardian.ComputeDiff(last, new)
	require.Len(t, again.ToTouch, 2)
	assert.Equal(t, diff.ToTouch[0].Key(), again.ToTouch[0].Key())
	assert.Equal(t, diff.ToTouch[1].Key(), again.ToTouch[1].Key())
}

// silence the unused-helper lint until ownerSet finds a caller.
var _ = ownerSet
