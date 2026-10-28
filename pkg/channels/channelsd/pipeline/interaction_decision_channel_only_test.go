package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestHandleInteractionDecision_ChannelDecisionOnly proves a
// ChannelDecisionOnly category refuses a self-asserted decider even when that
// decider has standing, while the same human on a channel transport resolves
// it. Standing is GRANTED here, so the refusal can only come from the flag.
// A plan card that only INCLUDES a channel-only consent is refused the same
// way, as not_authorized, rather than failing later as a handler error.
func TestHandleInteractionDecision_ChannelDecisionOnly(t *testing.T) {
	const includedCategory = "fixture_channel_only_consent"
	cases := []struct {
		name        string
		kind        identity.Kind
		viaPlanCard bool
		wantApplied bool
	}{
		{name: "CLI-asserted decider: not_authorized rejection, handler never runs", kind: identity.KindCLI},
		{name: "chat OS-username decider: not_authorized rejection, handler never runs", kind: identity.KindLocal},
		{name: "CLI-asserted decider on a plan card including the consent: not_authorized rejection", kind: identity.KindCLI, viaPlanCard: true},
		{name: "channel-authenticated decider: applied", kind: identity.KindSlack, wantApplied: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetInteractions(t)
			channelinteractions.Register(channelinteractions.Category{
				Name:                fixtureInteractionCategory,
				Park:                spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
				Tone:                channelinteractions.ToneRoutine,
				Deciders:            channelinteractions.DecideApprovers,
				Resurface:           channelinteractions.ResurfaceCached,
				ChannelDecisionOnly: !tc.viaPlanCard,
			})
			channelinteractions.Register(channelinteractions.Category{Name: includedCategory, ChannelDecisionOnly: true, PlanConsent: true, Tone: channelinteractions.TonePrivacy, Deciders: channelinteractions.DecideRequester, Resurface: channelinteractions.ResurfaceCached})
			calls := 0
			channelinteractions.Bind(fixtureInteractionCategory,
				trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))

			sessKey := sessKeyForFixture()
			natsRec := &fakeNATS{}
			p := newTestPipeline(t, newFakeK8sClientWithExistingSession(t, "U_ALICE"), &fakeAuthz{checkApproveResult: true}, natsRec)
			mem := newTestMemory(t)
			p.Mem = mem
			decider := channelevents.ExternalIdentity{Kind: tc.kind, ExternalID: "U_ALICE", Email: "alice@example.com"}
			request := approversFixtureRequest(decider)
			if tc.viaPlanCard {
				request.Consents = []channelevents.InteractionRequestPayload{{Category: includedCategory, RequestRef: "consent-1"}}
			}
			notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", request)

			env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", decider)
			require.NoError(t, p.HandleInteractionDecision(context.Background(), env))

			if tc.wantApplied {
				assert.Equal(t, 1, calls, "a channel decider resolves the request")
				assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied))
				return
			}
			assert.Equal(t, 0, calls, "the bound handler must not run for a self-asserted decider")
			assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied))
			assert.Equal(t, "not_authorized", findInteractionDecisionRejected(t, natsRec).Class)
			assert.Len(t, outstandingPrompts(t, p, sessKey), 1, "the prompt stays parked for the channel decision")
		})
	}
}
