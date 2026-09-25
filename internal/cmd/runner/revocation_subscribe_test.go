package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// fakeBus captures the subscriber handler so tests can fire revocation messages
// without a real NATS connection.
type fakeBus struct{ h func([]byte) }

func (f *fakeBus) Subscribe(_ string, handler func([]byte)) error {
	f.h = handler
	return nil
}

// fakeSecretInvalidator satisfies credential.SecretInvalidator for tests that
// don't exercise the credential revocation path.
type fakeSecretInvalidator struct{ calls []string }

func (f *fakeSecretInvalidator) InvalidateSecret(ns, name string) error {
	f.calls = append(f.calls, ns+"/"+name)
	return nil
}

func mkRevocationEnv(t *testing.T, kind, key, scope string) []byte {
	t.Helper()
	pl, err := json.Marshal(channelevents.RevokedPayload{Kind: kind, Key: key, Scope: scope})
	require.NoError(t, err)
	env, err := json.Marshal(channelevents.Envelope{Kind: channelevents.KindRevoked, Payload: pl})
	require.NoError(t, err)
	return env
}

// TestSubscribeRevocationOnBus verifies that:
//  1. A scope-matched tool-origin revocation invalidates the revokedOrigins set.
//  2. The onRevoked hook fires once per scope-matched revocation.
//  3. Out-of-scope messages are silently skipped (no invalidation, no hook).
//  4. The live-guard invalidation fires independently of the hook — the hook is
//     additive (used for audit event emission), not a gate on the guard path.
func TestSubscribeRevocationOnBus(t *testing.T) {
	revokedOrigins := toolorigin.New()
	credsInv := &fakeSecretInvalidator{}
	bus := &fakeBus{}

	type hookCall struct{ kind, key string }
	var hookCalls []hookCall
	reg, err := newRevocationRegistry(revokedOrigins, nil, credsInv)
	require.NoError(t, err)
	require.NoError(t, subscribeRevocationOnBus(
		context.Background(), bus, reg, "default",
		func(kind, key string) { hookCalls = append(hookCalls, hookCall{kind, key}) },
	))
	require.NotNil(t, bus.h, "bus.Subscribe must have been called")

	// In-scope revocation: revokedOrigins is mutated and the hook fires with kind+key.
	bus.h(mkRevocationEnv(t, "tool-origin", "mcpserver/code-tools", "default"))
	assert.True(t, revokedOrigins.IsRevoked("mcpserver/code-tools"),
		"tool origin must be revoked after in-scope message")
	assert.Equal(t, 1, len(hookCalls), "hook must fire once for in-scope revocation")
	assert.Equal(t, hookCall{"tool-origin", "mcpserver/code-tools"}, hookCalls[0])

	// Out-of-scope revocation: no invalidation, no hook.
	bus.h(mkRevocationEnv(t, "tool-origin", "mcpserver/other", "other-ns"))
	assert.False(t, revokedOrigins.IsRevoked("mcpserver/other"),
		"out-of-scope revocation must not affect revokedOrigins")
	assert.Equal(t, 1, len(hookCalls), "hook must not fire for out-of-scope revocation")

	// Cluster-wide revocation (empty scope) applies to every namespace.
	bus.h(mkRevocationEnv(t, "tool-origin", "mcpserver/global", ""))
	assert.True(t, revokedOrigins.IsRevoked("mcpserver/global"),
		"cluster-wide revocation must invalidate tool origin")
	assert.Equal(t, 2, len(hookCalls), "hook must fire once for cluster-wide revocation")
	assert.Equal(t, hookCall{"tool-origin", "mcpserver/global"}, hookCalls[1])
}

// A credential revoke must reach BOTH the broker's resolution cache and the
// frozen (header, value) copies inside live MCPTools. Dropping only the broker
// cache leaves the tool presenting the revoked bearer token upstream, because
// MCPTool never re-reads that cache — the runner kept authenticating with a
// revoked credential until the upstream happened to answer 401.
func TestSubscribeRevocationOnBus_CredentialRevokeReachesBrokerAndMCPTools(t *testing.T) {
	revokedOrigins := toolorigin.New()
	brokerInv := &fakeSecretInvalidator{}
	mcpAuthInv := newMCPAuthInvalidator()
	tool := &fakeAuthTool{}
	mcpAuthInv.register("id-ns", "gh-pat", tool)

	bus := &fakeBus{}
	reg, err := newRevocationRegistry(revokedOrigins, nil, brokerInv, mcpAuthInv)
	require.NoError(t, err)
	require.NoError(t, subscribeRevocationOnBus(context.Background(), bus, reg, "default", nil))
	require.NotNil(t, bus.h)

	// Credential revokes are published cluster-wide (scope "").
	bus.h(mkRevocationEnv(t, "credential", "id-ns/gh-pat", ""))

	assert.Equal(t, []string{"id-ns/gh-pat"}, brokerInv.calls, "broker cache must be dropped")
	assert.Equal(t, 1, tool.count(), "the MCPTool holding the revoked credential must be marked stale")
}
