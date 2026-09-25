package revocation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

type fakeNATS struct{ h func([]byte) }

func (f *fakeNATS) Subscribe(_ string, handler func([]byte)) error { f.h = handler; return nil }

func mkEnv(t *testing.T, kind, key, scope string) []byte {
	t.Helper()
	pl, err := json.Marshal(channelevents.RevokedPayload{Kind: kind, Key: key, Scope: scope})
	require.NoError(t, err)
	b, err := json.Marshal(channelevents.Envelope{Kind: channelevents.KindRevoked, Payload: pl})
	require.NoError(t, err)
	return b
}

func TestSubscriberDispatchesInScope(t *testing.T) {
	reg := NewRegistry()
	inv := &fakeInv{kind: "tool-origin"} // fakeInv defined in invalidator_test.go (same package)
	require.NoError(t, reg.Register(inv))
	bus := &fakeNATS{}
	require.NoError(t, RegisterSubscriber(context.Background(), bus, reg, "team-a"))

	bus.h(mkEnv(t, "tool-origin", "mcpserver/x", "team-a")) // in scope
	bus.h(mkEnv(t, "tool-origin", "mcpserver/y", "team-b")) // out of scope
	bus.h(mkEnv(t, "tool-origin", "mcpserver/z", ""))       // cluster-wide
	bus.h(mkEnv(t, "unknown", "k", ""))                     // unknown kind → skip

	assert.Equal(t, []string{"mcpserver/x", "mcpserver/z"}, inv.got)
}

// TestSubscriberHookFiresOncePerScopeMatchedRevocation verifies that the
// optional onRevoked hook is called exactly once for each scope-matched
// invalidation (in-scope and cluster-wide), NOT for out-of-scope messages or
// messages with unknown kinds, and NOT for malformed envelopes. The hook
// receives the revoked kind and key so callers can record them durably.
func TestSubscriberHookFiresOncePerScopeMatchedRevocation(t *testing.T) {
	reg := NewRegistry()
	inv := &fakeInv{kind: "tool-origin"}
	require.NoError(t, reg.Register(inv))
	bus := &fakeNATS{}

	type hookCall struct{ kind, key string }
	var calls []hookCall
	require.NoError(t, RegisterSubscriber(context.Background(), bus, reg, "team-a", func(kind, key string) {
		calls = append(calls, hookCall{kind, key})
	}))

	bus.h(mkEnv(t, "tool-origin", "mcpserver/x", "team-a")) // in scope → invalidates + hook
	bus.h(mkEnv(t, "tool-origin", "mcpserver/y", "team-b")) // out of scope → no hook
	bus.h(mkEnv(t, "tool-origin", "mcpserver/z", ""))       // cluster-wide → invalidates + hook
	bus.h(mkEnv(t, "unknown", "k", ""))                     // unknown kind → no invalidator → no hook
	bus.h([]byte("not-json"))                               // malformed → no hook

	assert.Equal(t, 2, len(calls), "hook fires once per scope-matched invalidation")
	assert.Equal(t, hookCall{"tool-origin", "mcpserver/x"}, calls[0])
	assert.Equal(t, hookCall{"tool-origin", "mcpserver/z"}, calls[1])
	assert.Equal(t, []string{"mcpserver/x", "mcpserver/z"}, inv.got)
}

// TestSubscriberHookSkippedWhenInvalidateFails: a revocation that did not
// reach every holder must NOT be reported as applied. The runner's hook is
// loop.EmitRevoked, which appends a durable, signed lifecycle record; firing
// it after a failed Invalidate writes "this credential was revoked" into the
// tamper-evident log while a live copy of that credential — the (header,
// value) an MCPTool froze at session start — keeps authenticating upstream.
// kinds/credential.Invalidator states the rule this enforces: a revocation
// that reached only some holders is strictly worse than one that reports
// failure, because the caller believes the credential is dead.
func TestSubscriberHookSkippedWhenInvalidateFails(t *testing.T) {
	reg := NewRegistry()
	inv := &fakeInv{kind: "credential", err: errors.New("no invalidation targets registered")}
	require.NoError(t, reg.Register(inv))
	bus := &fakeNATS{}

	var calls int
	require.NoError(t, RegisterSubscriber(context.Background(), bus, reg, "team-a", func(string, string) {
		calls++
	}))

	bus.h(mkEnv(t, "credential", "team-a/upstream-token", "team-a"))

	assert.Zero(t, calls, "a failed invalidation must not be recorded as applied")
	assert.Equal(t, []string{"team-a/upstream-token"}, inv.got, "the invalidation is still attempted; only the success record is withheld")
}
