package main

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAuthTool struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeAuthTool) InvalidateAuth() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
}

func (f *fakeAuthTool) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A credential revoke names one Secret. Only the tools whose auth header was
// resolved from that Secret may be marked stale — invalidating every tool would
// deny unrelated MCP servers whose credentials are perfectly valid.
func TestMCPAuthInvalidator_InvalidatesOnlyToolsBoundToThatSecret(t *testing.T) {
	inv := newMCPAuthInvalidator()
	linear, github := &fakeAuthTool{}, &fakeAuthTool{}
	inv.register("id-ns", "linear-oauth", linear)
	inv.register("id-ns", "gh-pat", github)

	require.NoError(t, inv.InvalidateSecret("id-ns", "linear-oauth"))

	assert.Equal(t, 1, linear.count(), "the tool bound to the revoked Secret must be invalidated")
	assert.Equal(t, 0, github.count(), "a tool bound to a different Secret must be untouched")
}

// Two MCP servers can share one backing Secret (e.g. a per-session passthrough
// Secret). Revoking it must reach both.
func TestMCPAuthInvalidator_InvalidatesEveryToolSharingASecret(t *testing.T) {
	inv := newMCPAuthInvalidator()
	a, b := &fakeAuthTool{}, &fakeAuthTool{}
	inv.register("id-ns", "shared", a)
	inv.register("id-ns", "shared", b)

	require.NoError(t, inv.InvalidateSecret("id-ns", "shared"))

	assert.Equal(t, 1, a.count())
	assert.Equal(t, 1, b.count())
}

// Namespace is part of the identity: the same Secret name in two namespaces is
// two distinct credentials.
func TestMCPAuthInvalidator_NamespaceIsPartOfTheKey(t *testing.T) {
	inv := newMCPAuthInvalidator()
	nsA, nsB := &fakeAuthTool{}, &fakeAuthTool{}
	inv.register("team-a", "creds", nsA)
	inv.register("team-b", "creds", nsB)

	require.NoError(t, inv.InvalidateSecret("team-a", "creds"))

	assert.Equal(t, 1, nsA.count())
	assert.Equal(t, 0, nsB.count(), "same Secret name in another namespace is a different credential")
}

// Revocation delivery is at-most-once and idempotent; an unknown Secret must be
// a safe no-op rather than an error that gets logged as a failure every time.
func TestMCPAuthInvalidator_UnknownSecretIsNoOp(t *testing.T) {
	inv := newMCPAuthInvalidator()
	assert.NoError(t, inv.InvalidateSecret("id-ns", "never-registered"))
}
