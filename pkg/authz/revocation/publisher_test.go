package revocation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

type capBus struct{ envs []channelevents.Envelope }

func (c *capBus) Publish(_ context.Context, e channelevents.Envelope) error {
	c.envs = append(c.envs, e)
	return nil
}

func decodePayload(t *testing.T, e channelevents.Envelope, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(e.Payload, v))
}

func TestEmitBuildsRevokedEnvelope(t *testing.T) {
	bus := &capBus{}
	p := NewPublisher(bus)
	require.NoError(t, p.Emit(context.Background(), "tool-origin", "mcpserver/x", "team-a"))

	require.Len(t, bus.envs, 1)
	assert.Equal(t, channelevents.KindRevoked, bus.envs[0].Kind)
	var pl channelevents.RevokedPayload
	decodePayload(t, bus.envs[0], &pl)
	assert.Equal(t, channelevents.RevokedPayload{Kind: "tool-origin", Key: "mcpserver/x", Scope: "team-a"}, pl)
}

func TestEmitNilBusIsNoOp(t *testing.T) {
	p := NewPublisher(nil)
	assert.NoError(t, p.Emit(context.Background(), "credential", "id-ns/sec", "id-ns"))
}
