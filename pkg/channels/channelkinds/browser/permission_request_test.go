package browser

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestPermissionRequestSender_Send(t *testing.T) {
	cases := []struct {
		name string
		env  func(t *testing.T) channelevents.Envelope
		want any
	}{
		{
			name: "permission_request: emits MsgPermissionRequest",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindPermissionRequest,
					channelevents.PermissionRequestPayload{
						AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "s1"},
						Preview:         "Bob wants to join",
					})
			},
			want: MsgPermissionRequest{},
		},
		{
			name: "permission_decision_applied: emits MsgPermissionDecisionApplied",
			env: func(t *testing.T) channelevents.Envelope {
				return mustEnv(t, channelevents.KindPermissionDecisionApplied,
					channelevents.PermissionDecisionAppliedPayload{Decision: "approve"})
			},
			want: MsgPermissionDecisionApplied{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &RecordingSink{}
			s := &permissionRequestSender{sink: sink}
			_, err := s.Send(context.Background(), sessInfo(), tc.env(t))
			require.NoError(t, err)
			require.Len(t, sink.Events(), 1)
			assert.IsType(t, tc.want, sink.Events()[0])
		})
	}
}
