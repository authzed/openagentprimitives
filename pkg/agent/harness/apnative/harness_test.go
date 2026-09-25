package apnative_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/apnative"
	"github.com/authzed/openagentprimitives/pkg/agent/harness/registry"
)

// NOTE: apnative is imported normally above, so its init() has already run by
// the time these tests execute. Do NOT add a second blank import of the same
// path — Go rejects the duplicate.

func TestAPNativeContributesNothingBeyondTheImage(t *testing.T) {
	h := apnative.Harness{}
	assert.Equal(t, harness.DefaultName, h.Name())
	assert.Equal(t, harness.Proxied, h.ModelAccess())

	spec, err := h.Container(harness.HarnessOpts{Image: "runner:dev"})
	require.NoError(t, err)
	assert.Equal(t, harness.ContainerSpec{}, spec,
		"ap-native must contribute an empty spec so the runner container is byte-identical to before the seam existed")
}

func TestAPNativeSelfRegisters(t *testing.T) {
	got, err := registry.Resolve("")
	require.NoError(t, err, "the blank import must have registered ap-native under the default name")
	assert.Equal(t, harness.DefaultName, got.Name())
}
