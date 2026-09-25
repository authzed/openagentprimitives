package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// opFakeBus captures the subscriber handler so tests can fire revocation
// messages without a real NATS connection.
type opFakeBus struct{ h func([]byte) }

func (f *opFakeBus) Subscribe(_ string, handler func([]byte)) error {
	f.h = handler
	return nil
}

// opFakeBroker records InvalidateSecret calls, standing in for the operator's
// inproc broker (whose cached token resolutions feed every sandbox ToolCall).
type opFakeBroker struct{ calls []string }

func (f *opFakeBroker) InvalidateSecret(ns, name string) error {
	f.calls = append(f.calls, ns+"/"+name)
	return nil
}

func mkOpRevocationEnv(t *testing.T, kind, key, scope string) []byte {
	t.Helper()
	pl, err := json.Marshal(channelevents.RevokedPayload{Kind: kind, Key: key, Scope: scope})
	require.NoError(t, err)
	env, err := json.Marshal(channelevents.Envelope{Kind: channelevents.KindRevoked, Payload: pl})
	require.NoError(t, err)
	return env
}

// The operator PUBLISHES revocation envelopes but historically never SUBSCRIBED
// to them, so its own token broker — the one the ToolCall reconciler calls to
// resolve credentials for every sandbox exec — was never invalidated. Combined
// with the broker caching static credentials with a zero (never) expiry, a
// revoked credential kept being injected into every NEWLY created ToolCall for
// the lifetime of the operator process. This is the most severe of the three
// revocation gaps because it affects new calls, not just in-flight ones.
func TestSubscribeRevocationOnBus_OperatorInvalidatesBrokerCache(t *testing.T) {
	b := &opFakeBroker{}
	bus := &opFakeBus{}

	require.NoError(t, subscribeRevocationOnBus(context.Background(), bus, b))
	require.NotNil(t, bus.h, "bus.Subscribe must have been called")

	// Cluster-wide credential revoke (how AgentIdentity/UserIdentity emit today).
	bus.h(mkOpRevocationEnv(t, "credential", "id-ns/gh-pat", ""))
	assert.Equal(t, []string{"id-ns/gh-pat"}, b.calls,
		"a cluster-wide credential revoke must drop the operator broker's cached token")

	// Namespace-scoped credential revoke. The operator's broker caches
	// credentials on behalf of sessions in EVERY namespace, so it must act on
	// this too — it subscribes as revocation.AllNamespaces. Were the operator to
	// pass its own namespace instead, this revoke would be silently dropped.
	bus.h(mkOpRevocationEnv(t, "credential", "team-a/linear-oauth", "team-a"))
	assert.Equal(t, []string{"id-ns/gh-pat", "team-a/linear-oauth"}, b.calls,
		"a namespace-scoped credential revoke must still reach the cluster-wide operator broker")
}

// The operator holds no tool-origin state (that lives in each runner's
// in-process set), so a tool-origin envelope must be skipped gracefully rather
// than crash or be misrouted to the credential invalidator.
func TestSubscribeRevocationOnBus_OperatorIgnoresToolOriginKind(t *testing.T) {
	b := &opFakeBroker{}
	bus := &opFakeBus{}
	require.NoError(t, subscribeRevocationOnBus(context.Background(), bus, b))

	bus.h(mkOpRevocationEnv(t, "tool-origin", "mcpserver/code-tools", "default"))
	assert.Empty(t, b.calls, "a tool-origin revoke must not be routed to the credential invalidator")
}

// Malformed envelopes must not panic the operator's subscriber goroutine.
func TestSubscribeRevocationOnBus_OperatorSurvivesMalformedEnvelope(t *testing.T) {
	b := &opFakeBroker{}
	bus := &opFakeBus{}
	require.NoError(t, subscribeRevocationOnBus(context.Background(), bus, b))

	assert.NotPanics(t, func() { bus.h([]byte("{not json")) })
	assert.Empty(t, b.calls)
}
