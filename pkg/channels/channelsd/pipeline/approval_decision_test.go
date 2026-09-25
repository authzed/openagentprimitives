package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

func TestApprovalDecisionHandler(t *testing.T) {
	cases := []struct {
		name       string
		actionID   string
		wantResult string
		wantErr    bool
	}{
		{"approve action → approved outcome", "approve", channelevents.OutcomeApproved, false},
		{"deny action → denied outcome", "deny", channelevents.OutcomeDenied, false},
		{"unknown action → error", "maybe", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ApprovalDecisionHandler(context.Background(), channelinteractions.Decision{
				Payload: channelevents.InteractionDecisionPayload{Category: "content_inspection", ActionID: tc.actionID},
			})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantResult, out.Result)
			assert.NoError(t, out.Validate())
		})
	}
}
