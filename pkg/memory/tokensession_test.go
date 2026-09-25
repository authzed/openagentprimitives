package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestTokenSessionFrom_AbsentOnAPlainContext(t *testing.T) {
	sess, ok := memory.TokenSessionFrom(context.Background())
	assert.False(t, ok, "a context that never saw a per-session token reports absent")
	assert.Equal(t, memory.NamespacedName{}, sess)
}

func TestTokenSessionFrom_RoundTrip(t *testing.T) {
	want := memory.NamespacedName{Namespace: "ns", Name: "sess-a"}
	sess, ok := memory.TokenSessionFrom(memory.WithTokenSession(context.Background(), want))
	assert.True(t, ok)
	assert.Equal(t, want, sess)
}

// TestTokenSessionFrom_EmptyIsPresentNotAbsent is the whole contract. Unlike
// CallerFrom — which deliberately collapses "" into absent — an unnamed token
// session must stay PRESENT, so the consumer sees a wiring bug and fails closed
// instead of quietly falling through to the weaker "no token" path. Collapsing
// the two is exactly the defect the writer-binding was suffering from.
func TestTokenSessionFrom_EmptyIsPresentNotAbsent(t *testing.T) {
	sess, ok := memory.TokenSessionFrom(memory.WithTokenSession(context.Background(), memory.NamespacedName{}))
	assert.True(t, ok, "a present-but-unnamed token session must NOT read as absent")
	assert.Equal(t, memory.NamespacedName{}, sess)
}

func TestWithoutTokenSession_ClearsTheMarkAndIsIdempotent(t *testing.T) {
	marked := memory.WithTokenSession(context.Background(), memory.NamespacedName{Namespace: "ns", Name: "sess-a"})

	cleared := memory.WithoutTokenSession(marked)
	_, ok := memory.TokenSessionFrom(cleared)
	assert.False(t, ok, "the hand-off strips the token holder's identity")

	_, ok = memory.TokenSessionFrom(memory.WithoutTokenSession(cleared))
	assert.False(t, ok, "clearing an already-clear context is a no-op, not a re-mark")

	_, stillMarked := memory.TokenSessionFrom(marked)
	assert.True(t, stillMarked, "clearing derives a new context; the original is untouched")
}
