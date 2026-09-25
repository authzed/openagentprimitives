package channelevents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestRestartTriggerPayload_Validate(t *testing.T) {
	cases := []struct {
		name    string
		payload channelevents.RestartTriggerPayload
		wantErr string
	}{
		{
			name: "valid",
			payload: channelevents.RestartTriggerPayload{
				CutTurnIndex:      3,
				NewUserText:       "edited",
				TriggeredBy:       "user:alice",
				TargetSessionName: "sess-fk-abc",
			},
		},
		{
			name:    "missing TriggeredBy",
			payload: channelevents.RestartTriggerPayload{CutTurnIndex: 3, NewUserText: "x", TargetSessionName: "y"},
			wantErr: "TriggeredBy",
		},
		{
			name:    "missing TargetSessionName",
			payload: channelevents.RestartTriggerPayload{CutTurnIndex: 3, NewUserText: "x", TriggeredBy: "user:a"},
			wantErr: "TargetSessionName",
		},
		{
			name:    "missing NewUserText",
			payload: channelevents.RestartTriggerPayload{CutTurnIndex: 3, TriggeredBy: "u", TargetSessionName: "t"},
			wantErr: "NewUserText",
		},
		{
			name:    "negative CutTurnIndex",
			payload: channelevents.RestartTriggerPayload{CutTurnIndex: -1, NewUserText: "x", TriggeredBy: "u", TargetSessionName: "t"},
			wantErr: "CutTurnIndex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.payload.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestKindRestartTrigger_Const(t *testing.T) {
	assert.Equal(t, channelevents.Kind("restart_trigger"), channelevents.KindRestartTrigger)
}
