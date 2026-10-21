package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestBindLoginHumanStepHandler proves the decision pipe can resolve a handler
// for login_human_step after binding — without it, HandleInteractionDecision
// returns "no bound decision handler" and request_human_step waits out its full
// approval timeout — and that the bound handler is a pure approve/deny (no side
// effect, like content_inspection). Resets the global binding registry around the
// test, the established pipeline pattern (never t.Parallel).
func TestBindLoginHumanStepHandler(t *testing.T) {
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)

	// nil *Pipeline is accepted: the confirm handler reads no pipeline deps today.
	BindLoginHumanStepHandler(nil)

	h, ok := channelinteractions.HandlerFor(categories.LoginHumanStep)
	require.True(t, ok, "login_human_step must have a bound decision handler or the pipe errors and the session hangs")

	cases := []struct {
		actionID string
		want     string
	}{
		{"approve", channelevents.OutcomeApproved},
		{"deny", channelevents.OutcomeDenied},
	}
	for _, tc := range cases {
		out, err := h(context.Background(), channelinteractions.Decision{
			Payload: channelevents.InteractionDecisionPayload{
				Category: categories.LoginHumanStep, RequestRef: "req-1", ActionID: tc.actionID,
			},
		})
		require.NoError(t, err, "actionID %q", tc.actionID)
		assert.Equal(t, tc.want, out.Result, "actionID %q", tc.actionID)
		assert.NoError(t, out.Validate())
	}
}

// TestLoginHumanStepDecisionHandler_CarriesCodeOnApproveDropsOnDeny is the C7
// channelsd half: an approve carries the decider's typed code through to the
// runner on Outcome.ResponseText (never OutcomeText, never a rendered field); a
// deny drops it (a declined card types nothing).
func TestLoginHumanStepDecisionHandler_CarriesCodeOnApproveDropsOnDeny(t *testing.T) {
	channelinteractions.ResetBindings()
	t.Cleanup(channelinteractions.ResetBindings)
	BindLoginHumanStepHandler(nil)
	h, ok := channelinteractions.HandlerFor(categories.LoginHumanStep)
	require.True(t, ok)

	t.Run("approve carries the typed code as free text", func(t *testing.T) {
		out, err := h(context.Background(), channelinteractions.Decision{
			Payload: channelevents.InteractionDecisionPayload{
				Category: categories.LoginHumanStep, RequestRef: "req-1",
				ActionID: "approve", ResponseText: "123456",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, channelevents.OutcomeApproved, out.Result)
		assert.Equal(t, "123456", out.ResponseText, "the typed code must ride ResponseText to the runner")
		assert.Empty(t, out.OutcomeText, "the code must NOT be smuggled through the closed OutcomeText")
	})

	t.Run("deny drops the code", func(t *testing.T) {
		out, err := h(context.Background(), channelinteractions.Decision{
			Payload: channelevents.InteractionDecisionPayload{
				Category: categories.LoginHumanStep, RequestRef: "req-1",
				ActionID: "deny", ResponseText: "123456",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, channelevents.OutcomeDenied, out.Result)
		assert.Empty(t, out.ResponseText, "a declined card must carry no code to type")
	})
}
