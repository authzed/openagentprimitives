package kindtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

func TestSampleRequestIsValidForEveryPolicyShape(t *testing.T) {
	cases := []channelinteractions.Category{
		{Name: "parking_approver_cat", Park: v1alpha1.AgentSessionPhaseAwaitingDecision, Deciders: channelinteractions.DecideApprovers, Resurface: channelinteractions.ResurfaceCached},
		{Name: "requester_link_cat", Deciders: channelinteractions.DecideRequester, Resurface: channelinteractions.ResurfaceNone},
		{Name: "owner_regen_cat", Park: v1alpha1.AgentSessionPhaseAwaitingCredentials, Deciders: channelinteractions.DecideOwner, Resurface: channelinteractions.ResurfaceRegenerate},
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			req := SampleRequest(c)
			assert.Equal(t, c.Name, req.Category)
			require.NoError(t, req.Validate(), "SampleRequest must produce a wire-valid payload")
		})
	}
}

func TestAssertRegisteredCategoriesCoversTheLiveRegistry(t *testing.T) {
	channelinteractions.Reset()
	t.Cleanup(channelinteractions.Reset)
	channelinteractions.Register(channelinteractions.Category{
		Name: "fixture_cat", Park: v1alpha1.AgentSessionPhaseAwaitingDecision,
		Deciders: channelinteractions.DecideApprovers, Tone: channelinteractions.ToneRoutine,
		Resurface: channelinteractions.ResurfaceCached,
	})

	// Must not fail for a well-formed registry.
	AssertRegisteredCategories(t)
}
