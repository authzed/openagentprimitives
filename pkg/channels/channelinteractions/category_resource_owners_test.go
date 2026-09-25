package channelinteractions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestDecideResourceOwners_Validates(t *testing.T) {
	c := Category{Name: "tool_approval", Park: v1alpha1.AgentSessionPhaseAwaitingDecision, Deciders: DecideResourceOwners, Tone: ToneRoutine, Resurface: ResurfaceCached}
	require.NoError(t, c.Validate())
	assert.Equal(t, DeciderPolicy("resource_owners"), DecideResourceOwners)
}
