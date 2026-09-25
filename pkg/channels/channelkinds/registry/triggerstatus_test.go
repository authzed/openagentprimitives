package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// TestTriggerStatusReporterFor_AgreesWithTheKindItself is derived from the
// registry rather than from a list of names, so a kind that implements the seam
// later is covered without editing this test — the property the lookup exists
// to give its callers.
func TestTriggerStatusReporterFor_AgreesWithTheKindItself(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds,
		"the kind registry must be populated by init — an empty registry would make every row below vacuous")

	var anyReporter, anyNonReporter bool
	for _, k := range kinds {
		_, want := k.(channelkinds.TriggerStatusReporter)
		anyReporter = anyReporter || want
		anyNonReporter = anyNonReporter || !want
		t.Run(k.Name(), func(t *testing.T) {
			got, ok := registry.TriggerStatusReporterFor(k.Name())
			assert.Equal(t, want, ok,
				"TriggerStatusReporterFor must answer exactly what the kind's own type does")
			if want {
				assert.NotNil(t, got, "a reporting kind must come back with a usable reporter")
			} else {
				assert.Nil(t, got, "a non-reporting kind must come back nil, not a typed-nil interface")
			}
		})
	}
	assert.True(t, anyReporter,
		"at least one registered kind must report trigger status, or the equality above holds only in the false direction")
	assert.True(t, anyNonReporter,
		"at least one registered kind must NOT, or the equality above holds only in the true direction")
}

// TestTriggerStatusReporterFor_UnregisteredKindReportsFalse: callers hold a
// denormalized kind string off a ChannelBinding, and an unlinked kind is a
// wiring bug that surfaces loudly elsewhere. Answering "no reporter" keeps this
// from becoming a second, differently-shaped failure for the same bug.
func TestTriggerStatusReporterFor_UnregisteredKindReportsFalse(t *testing.T) {
	for _, name := range []string{"nosuchkind", ""} {
		got, ok := registry.TriggerStatusReporterFor(name)
		assert.False(t, ok, "kind %q", name)
		assert.Nil(t, got, "kind %q", name)
	}
}
