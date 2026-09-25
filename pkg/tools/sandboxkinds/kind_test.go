package sandboxkinds_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// The Feature set is deliberately closed: Feature answers only questions asked
// at class-validation time, where no Runtime exists. Runtime verbs (snapshot,
// suspend, file transfer) are optional interfaces instead, so there is exactly
// one home for each fact. A new Feature constant that is not a
// class-validation question means that split has eroded.
func TestClassValidationFeatures_IsTheClosedSet(t *testing.T) {
	got := sandboxkinds.ClassValidationFeatures()
	require.Len(t, got, 5, "ClassValidationFeatures must list every Feature constant")

	assert.ElementsMatch(t, []sandboxkinds.Feature{
		sandboxkinds.FeatureSharedWorkspace,
		sandboxkinds.FeatureConfigMapMounts,
		sandboxkinds.FeatureToolchainOverlay,
		sandboxkinds.FeatureHostEgressAllowlist,
		sandboxkinds.FeatureUnpackMounts,
	}, got)
}

func TestPhase_Values(t *testing.T) {
	assert.Equal(t, sandboxkinds.Phase("Pending"), sandboxkinds.PhasePending)
	assert.Equal(t, sandboxkinds.Phase("Ready"), sandboxkinds.PhaseReady)
	assert.Equal(t, sandboxkinds.Phase("Failed"), sandboxkinds.PhaseFailed)
	assert.Equal(t, sandboxkinds.Phase("Gone"), sandboxkinds.PhaseGone)
}

// A Handle must survive the round trip through its persisted CRD form with
// EVERY field intact. Written reflectively rather than as a field list because
// the bug it guards against is a new Handle field carried at the write site
// and forgotten at one of the three read sites — and a hand-written list here
// would need the very edit that was forgotten. A field type this test does not
// know how to populate fails loudly, which is the prompt to extend both the
// conversion and this test together.
func TestHandle_RoundTripsThroughStatusWithNoFieldLost(t *testing.T) {
	var h sandboxkinds.Handle
	v := reflect.ValueOf(&h).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("round-trip-" + v.Type().Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		default:
			t.Fatalf("Handle.%s has type %s, which this test cannot populate: extend "+
				"HandleFromStatus/ToStatus and this switch together",
				v.Type().Field(i).Name, f.Type())
		}
	}

	assert.Equal(t, h, sandboxkinds.HandleFromStatus(h.ToStatus()),
		"every Handle field must survive being persisted to and read back from "+
			"SpiceboxSession.status.sandbox — Prewarmed selects a whole resolution path, "+
			"so dropping it sends Status/Teardown/Executor at the wrong object")
}

// A nil stored handle is the zero Handle, which ResolveHandle then refuses. A
// session that never bound to a backend must not yield a handle that some
// kind's Ref parser happens to accept.
func TestHandleFromStatus_NilIsTheZeroHandle(t *testing.T) {
	assert.Equal(t, sandboxkinds.Handle{}, sandboxkinds.HandleFromStatus(nil))
}
