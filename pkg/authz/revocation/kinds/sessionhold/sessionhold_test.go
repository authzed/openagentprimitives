package sessionhold

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKind_ReportsWireIdentity(t *testing.T) {
	k := New("demo-ns/demo-session", func() {})
	assert.Equal(t, "session-hold", k.Kind())
}

// The noun is substituted into chat copy a surface renders as trusted markup,
// so it has to be a phrase a person recognises — not the wire token Kind()
// returns, and not internal vocabulary like "invalidator" or "revoke".
func TestKind_NounIsUserFacingAndOwnsNoInternalVocabulary(t *testing.T) {
	noun := New("demo-ns/demo-session", func() {}).Noun()
	assert.Equal(t, "a held session", noun)
	for _, internal := range []string{"session-hold", "invalidat", "revoke"} {
		assert.NotContains(t, noun, internal,
			"internal vocabulary %q must not ride into user copy on the noun", internal)
	}
}

func TestInvalidate_stopsTheLoopForItsOwnSession(t *testing.T) {
	stopped := 0
	k := New("demo-ns/demo-session", func() { stopped++ })
	require.NoError(t, k.Invalidate("demo-ns/demo-session"))
	assert.Equal(t, 1, stopped)
}

func TestInvalidate_isIdempotent(t *testing.T) {
	// At-most-once delivery means a redelivery is possible and a no-op must be
	// safe; a second stop must not error.
	stopped := 0
	k := New("demo-ns/demo-session", func() { stopped++ })
	require.NoError(t, k.Invalidate("demo-ns/demo-session"))
	require.NoError(t, k.Invalidate("demo-ns/demo-session"))
	assert.Equal(t, 2, stopped, "stop is idempotent at the loop; the invalidator does not dedupe")
}

func TestInvalidate_nilStopIsSafe(t *testing.T) {
	k := New("demo-ns/demo-session", nil)
	require.NoError(t, k.Invalidate("demo-ns/demo-session"))
}

// TestInvalidate_ignoresAnotherSessionsKey pins the fan-out precision this
// kind needs that credential/toolorigin get for free from their key-parsing
// Invalidate. The ap.revocation subject is one flat, cluster-wide stream that
// revocation.Applies filters only by NAMESPACE, not by session — so every
// runner process in a shared namespace receives every session-hold revoke
// published there, sibling sessions included. Each runner registers its own
// Kind bound to its own loop's stop func; without this key check, holding one
// session would stop every other concurrently-running session sharing that
// namespace (or, at cluster-wide scope, every session in the cluster).
func TestInvalidate_ignoresAnotherSessionsKey(t *testing.T) {
	stopped := 0
	k := New("demo-ns/demo-session", func() { stopped++ })
	require.NoError(t, k.Invalidate("demo-ns/some-other-session"))
	assert.Equal(t, 0, stopped, "a revoke naming a different session must not stop this one")
}
