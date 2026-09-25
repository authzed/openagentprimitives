package spicedbauthorizer_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	artifactkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
)

func TestEntryResourceID(t *testing.T) {
	e := memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "default/my-session"},
		Kind:  "turn",
		ID:    "turn-abc123",
	}
	assert.Equal(t, "session/default/my-session/turn/turn-abc123",
		spicedbauthorizer.EntryResourceID(e))
}

func TestEntryResourceIDFromParts(t *testing.T) {
	scope := memory.Scope{Kind: "session", ID: "default/my-session"}
	assert.Equal(t, "session/default/my-session/turn/turn-abc123",
		spicedbauthorizer.EntryResourceIDFromParts(scope, "turn", "turn-abc123"))
}

func TestScopePrefix(t *testing.T) {
	scope := memory.Scope{Kind: "session", ID: "default/my-session"}
	assert.Equal(t, "session/default/my-session/",
		spicedbauthorizer.ScopePrefix(scope))
}

func TestAuthorizePut_NoCaller_Allows(t *testing.T) {
	az := spicedbauthorizer.New(nil)
	err := az.AuthorizePut(context.Background(), memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/s"},
		Kind:  "turn",
		ID:    "turn-1",
	})
	assert.NoError(t, err, "no caller in context should allow unconditionally")
}

func TestAuthorizeQuery_NoCaller_ReturnsAll(t *testing.T) {
	az := spicedbauthorizer.New(nil)
	entries := []memory.Entry{
		{Kind: "turn", ID: "turn-1"},
		{Kind: "turn", ID: "turn-2"},
	}
	result, err := az.AuthorizeQuery(context.Background(), entries)
	require.NoError(t, err)
	assert.Len(t, result, 2, "no caller should return all entries")
}

func TestAuthorizeQuery_SystemCaller_ReturnsAll(t *testing.T) {
	entries := []memory.Entry{
		{Kind: "turn", ID: "turn-1"},
		{Kind: "turn", ID: "turn-2"},
	}
	for _, caller := range []string{"system:channelsd", "system:authzd"} {
		t.Run(caller+" returns all entries without a SpiceDB check", func(t *testing.T) {
			az := spicedbauthorizer.New(nil)
			ctx := memory.WithCaller(context.Background(), caller)
			result, err := az.AuthorizeQuery(ctx, entries)
			require.NoError(t, err)
			assert.Len(t, result, 2, "system caller should return all entries unfiltered")
		})
	}
}

func TestAuthorizeQuery_EmptyEntries(t *testing.T) {
	az := spicedbauthorizer.New(nil)
	ctx := memory.WithCaller(context.Background(), "user-1")
	result, err := az.AuthorizeQuery(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// TestAuthorizePut_CallerBypass tables the caller→bypass contract for
// AuthorizePut on a non-artifact entry. System callers (any "system:" prefix)
// MUST bypass the caller-scoped write (memory_entry#session/#creator); a normal
// base64 user caller MUST attempt the write. (Artifact entries are different —
// artifact#parent is written regardless of caller; see the test below.) The
// authorizer is constructed with a nil client, so reaching the write path
// nil-dereferences and panics — which is precisely how we prove, without a
// mock, that a system caller takes zero SpiceDB calls (no panic) while a user
// caller does reach the client (panic).
func TestAuthorizePut_CallerBypass(t *testing.T) {
	entry := memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/s"},
		Kind:  "turn",
		ID:    "turn-1",
	}
	cases := []struct {
		name       string
		caller     string // "" → no caller in ctx
		wantBypass bool   // true → no SpiceDB call (returns nil, no panic)
	}{
		{name: "system:channelsd bypasses the SpiceDB write", caller: "system:channelsd", wantBypass: true},
		{name: "system:authzd bypasses the SpiceDB write", caller: "system:authzd", wantBypass: true},
		{name: "no caller bypasses the SpiceDB write", caller: "", wantBypass: true},
		// Canonical user IDs are base64 (no colon); this caller is NOT a
		// system caller and must reach the (nil) client → panic.
		{name: "base64 user caller reaches the SpiceDB write", caller: "dXNlci1hbGljZQ==", wantBypass: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			az := spicedbauthorizer.New(nil)
			ctx := context.Background()
			if tc.caller != "" {
				ctx = memory.WithCaller(ctx, tc.caller)
			}
			if tc.wantBypass {
				assert.NotPanics(t, func() {
					assert.NoError(t, az.AuthorizePut(ctx, entry),
						"bypassing caller must return nil without touching SpiceDB")
				}, "bypassing caller must not reach the SpiceDB client")
				return
			}
			assert.Panics(t, func() {
				_ = az.AuthorizePut(ctx, entry)
			}, "non-system user caller must reach the SpiceDB client (nil client → panic)")
		})
	}
}

// TestAuthorizePut_ArtifactParent_WrittenRegardlessOfCaller guards the fix for
// the silent-drop bug: artifact#parent is caller-INDEPENDENT (artifact#view =
// parent->interact gates every session participant, not the writer). The agent
// runner persists artifacts as a SYSTEM caller, so the old
// `if !ok || isSystemCaller { return nil }` silently skipped the parent write
// and made every live-view artifact unviewable with zero diagnostics. The fix
// reaches the parent-write path for ALL callers; with a nil client it surfaces
// a LOUD error rather than silently returning nil.
func TestAuthorizePut_ArtifactParent_WrittenRegardlessOfCaller(t *testing.T) {
	az := spicedbauthorizer.New(nil)
	artifact := memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/s"},
		Kind:  artifactkind.KindName,
		ID:    "artifact-abc",
	}
	for _, caller := range []string{"system:channelsd", "system:authzd", ""} {
		name := caller
		if name == "" {
			name = "no-caller"
		}
		t.Run(name+": artifact#parent reached, not silently skipped", func(t *testing.T) {
			ctx := context.Background()
			if caller != "" {
				ctx = memory.WithCaller(ctx, caller)
			}
			err := az.AuthorizePut(ctx, artifact)
			require.Error(t, err,
				"a system/no-caller artifact write MUST reach the parent write — never return nil silently")
			assert.Contains(t, err.Error(), "artifact#parent",
				"the error must name the artifact#parent write it could not complete")
		})
	}
}

func TestAuthorizeDelete_SystemCaller_Allows(t *testing.T) {
	az := spicedbauthorizer.New(nil)
	ctx := memory.WithCaller(context.Background(), "system:channelsd")
	err := az.AuthorizeDelete(ctx,
		memory.Scope{Kind: "session", ID: "ns/s"}, "turn", "turn-1")
	assert.NoError(t, err, "system caller should bypass AuthorizeDelete")
}

func TestAuthorizeDelete_NoCaller_Allows(t *testing.T) {
	az := spicedbauthorizer.New(nil)
	err := az.AuthorizeDelete(context.Background(),
		memory.Scope{Kind: "session", ID: "ns/s"}, "turn", "turn-1")
	assert.NoError(t, err, "no caller in context should allow unconditionally")
}

func TestSplitSessionScope(t *testing.T) {
	ns, name := spicedbauthorizer.SplitSessionScope("default/sess1")
	assert.Equal(t, "default", ns)
	assert.Equal(t, "sess1", name)

	ns, name = spicedbauthorizer.SplitSessionScope("weird")
	assert.Equal(t, "weird", ns)
	assert.Equal(t, "", name)
}
