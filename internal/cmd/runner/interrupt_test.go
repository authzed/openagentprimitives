package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestHandleInterruptRequest(t *testing.T) {
	l := &runner.Loop{}
	// nothing in flight → rejected
	env, err := channelevents.BuildEnvelope("chat", "s1", channelevents.KindInterruptRequest,
		channelevents.InterruptRequestPayload{RequestID: "r1"})
	require.NoError(t, err)
	data, err := json.Marshal(env)
	require.NoError(t, err)
	applied, ok := handleInterruptRequest(l, data)
	require.True(t, ok)
	assert.Equal(t, "r1", applied.RequestID)
	assert.Equal(t, "rejected", applied.Outcome)
	assert.NotEmpty(t, applied.Reason)

	// malformed (not even valid JSON) → ok false
	_, ok = handleInterruptRequest(l, []byte("{bad"))
	assert.False(t, ok, "malformed request is not published")
}

func TestHandleInterruptRequest_CopiesResponseURL(t *testing.T) {
	l := &runner.Loop{}
	env, err := channelevents.BuildEnvelope("chat", "s1", channelevents.KindInterruptRequest,
		channelevents.InterruptRequestPayload{
			RequestID:   "r1",
			ResponseURL: "https://hooks.slack.example/r1",
		})
	require.NoError(t, err)
	data, err := json.Marshal(env)
	require.NoError(t, err)

	applied, ok := handleInterruptRequest(l, data)
	require.True(t, ok)
	assert.Equal(t, "https://hooks.slack.example/r1", applied.ResponseURL)
}
