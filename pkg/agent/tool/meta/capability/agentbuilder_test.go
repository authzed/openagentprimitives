package capability

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sanction-gate uniqueness the spec demands (§1.1): agent_builder is the
// ONLY capability whose grant is decided outside the class's own spec. The
// walk is over the live registry, so a second sanction-gated capability —
// or losing the marker in a refactor — fails here by name.
func TestAgentBuilderIsTheOnlySanctionGatedCapability(t *testing.T) {
	type sanctionGated interface{ SanctionGated() bool }
	var gated []string
	for _, c := range Ordered() {
		if sg, ok := c.(sanctionGated); ok && sg.SanctionGated() {
			gated = append(gated, c.Name())
		}
	}
	assert.Equal(t, []string{"agent_builder"}, gated)
}

func TestAgentBuilderCapability_DefaultOnFalse(t *testing.T) {
	c, ok := Lookup("agent_builder")
	require.True(t, ok, "agent_builder must be registered")
	assert.False(t, c.DefaultOn(), "a class must opt in explicitly")
	assert.False(t, c.Infrastructural())
}

func TestAgentBuilderCapability_Offer(t *testing.T) {
	tools, skip := (agentBuilderCapability{}).Offer(OfferContext{Granted: false})
	assert.Nil(t, tools)
	assert.Nil(t, skip, "ungranted: no tool, no skip reason")

	tools, skip = (agentBuilderCapability{}).Offer(OfferContext{Granted: true})
	assert.Nil(t, tools, "granting the capability alone never contributes a tool")
	require.NotNil(t, skip)
	assert.Equal(t, "agent_builder", skip.Capability)
	assert.Contains(t, skip.Reason, "workshop sidecar")
}
