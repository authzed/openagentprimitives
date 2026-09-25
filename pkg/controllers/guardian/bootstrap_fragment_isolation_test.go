package guardian

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	guardianschema "github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// Two bootstraps declaring the same resource name with different bodies used to
// fail the union, and validateBootstraps stamped BOTH invalid. The partition
// keeps the first and rejects only the later one, so an operator's good
// bootstrap is not held hostage by a bad one added afterwards.
//
// This calls PartitionCompatibleFragments directly to pin ITS behavior in
// isolation — it does not exercise the controller's routing. The fixtures
// below are RawZed-only (no Resources), a shape the controller's own
// candidate-building loop in agentsessiongrants_controller.go's Reconcile
// DOES route here: its skip check is RawZed-aware (`frag == nil ||
// (len(frag.Resources) == 0 && frag.RawZed == "")`), matching the
// MCPServer/SidecarToolbox/SpiceboxToolkit loops — a real SpiceDBBootstrap
// carrying only RawZed reaches this partition exactly like a
// Resources-carrying one (see
// TestReconciler_ComposesRawZedOnlySpiceDBBootstrapFragment in
// agentsessiongrants_controller_test.go for the controller-level proof).
//
// Both fragments carry Tier: TierOperator — SpiceDBBootstrap is the one
// operator-authored kind (FragmentTier's doc comment), so a test pinning its
// isolation behavior should exercise that tier rather than the tenant zero
// value; TestPartition_WithinATierKeyStillDecides (partition_test.go)
// already covers the tenant-tier version of this same-tier tie-break.
func TestBootstrapFragmentsAreIsolatedNotBlanketRejected(t *testing.T) {
	first := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition tenant_thing {\n    relation owner: user\n    permission read = owner\n}\n",
	}
	second := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition tenant_thing {\n    relation viewer: user\n    permission read = viewer\n}\n",
	}

	accepted, rejected := guardianschema.PartitionCompatibleFragments(nil, []guardianschema.IdentifiedFragment{
		{Key: "spicedbbootstrap:default/a-first", Tier: guardianschema.TierOperator, Fragment: first},
		{Key: "spicedbbootstrap:default/b-second", Tier: guardianschema.TierOperator, Fragment: second},
	})

	require.Len(t, accepted, 1, "exactly one survives a same-name conflict")
	assert.Equal(t, "spicedbbootstrap:default/a-first", accepted[0].Key, "first in order wins")
	require.Len(t, rejected, 1)
	assert.Equal(t, "spicedbbootstrap:default/b-second", rejected[0].Key)
	require.Error(t, rejected[0].Err, "the rejection carries WHY, for the CR's condition message")
}
