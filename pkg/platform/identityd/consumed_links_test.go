package identityd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeConsumedBackend is an in-process stand-in for the durable Secret. It
// survives a store being rebuilt, which is how these tests model an identityd
// restart, and it records what was handed to it so a test can assert the raw
// link never reaches storage.
type fakeConsumedBackend struct {
	entries map[string]time.Time
	err     error
	// seen is every key the store asked the backend to record — the assertion
	// surface for "no raw link is ever persisted".
	seen []string
}

func newFakeConsumedBackend() *fakeConsumedBackend {
	return &fakeConsumedBackend{entries: map[string]time.Time{}}
}

func (f *fakeConsumedBackend) markConsumed(_ context.Context, key string, at time.Time, ttl time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.seen = append(f.seen, key)
	for k, t := range f.entries {
		if ttl >= 0 && t.Before(at.Add(-ttl)) {
			delete(f.entries, k)
		}
	}
	if _, already := f.entries[key]; already {
		return false, nil
	}
	f.entries[key] = at
	return true, nil
}

func TestConsumedLinkStore_FirstUseSucceedsSecondFails(t *testing.T) {
	s := newConsumedLinkStore(10*time.Minute, newFakeConsumedBackend())
	ok, err := s.markConsumed(context.Background(), "link-abc.sig-def")
	require.NoError(t, err)
	assert.True(t, ok, "first markConsumed must return true (was not already consumed)")
	ok, err = s.markConsumed(context.Background(), "link-abc.sig-def")
	require.NoError(t, err)
	assert.False(t, ok, "second markConsumed must return false (already consumed)")
}

func TestConsumedLinkStore_DifferentLinksAreIndependent(t *testing.T) {
	s := newConsumedLinkStore(10*time.Minute, newFakeConsumedBackend())
	ok, err := s.markConsumed(context.Background(), "a.b")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.markConsumed(context.Background(), "c.d")
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestConsumedLinkStore_ExpiredEntriesAllowRetry(t *testing.T) {
	s := newConsumedLinkStore(0, newFakeConsumedBackend()) // immediate expiry
	ok, err := s.markConsumed(context.Background(), "x.y")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.markConsumed(context.Background(), "x.y")
	require.NoError(t, err)
	assert.True(t, ok, "expired entry should allow re-consumption")
}

// TestConsumedLinkStore_SurvivesRestart is the durability regression test.
// The store is the ONLY thing enforcing single-use on a signed portal link —
// its call site defends the case where "Mallory forwards Alice's portal link,
// only one of them gets the cookie". Held purely in memory, an identityd
// restart (deploy, eviction, OOM) silently re-armed every already-consumed
// link for the remainder of its 30-minute signed validity.
//
// Rebuilding the store over the same backend is exactly what a restart does.
func TestConsumedLinkStore_SurvivesRestart(t *testing.T) {
	const link = "fwd-b64.fwd-sig"
	backend := newFakeConsumedBackend()

	before := newConsumedLinkStore(35*time.Minute, backend)
	ok, err := before.markConsumed(context.Background(), link)
	require.NoError(t, err)
	require.True(t, ok, "precondition: the first use consumes the link")

	// identityd restarts: a brand-new process-local store over the same
	// durable record.
	after := newConsumedLinkStore(35*time.Minute, backend)
	ok, err = after.markConsumed(context.Background(), link)
	require.NoError(t, err)
	assert.False(t, ok, "a link consumed before the restart must stay consumed after it")
}

// TestConsumedLinkStore_NeverPersistsTheRawLink guards the fix from becoming a
// worse problem than the bug: the signed link is bearer-shaped, so persisting
// it verbatim would turn the durable record into a credential store readable by
// anyone who can read the object. Only an opaque digest may leave the process.
func TestConsumedLinkStore_NeverPersistsTheRawLink(t *testing.T) {
	const link = "super-secret-payload.super-secret-signature"
	backend := newFakeConsumedBackend()
	s := newConsumedLinkStore(35*time.Minute, backend)

	_, err := s.markConsumed(context.Background(), link)
	require.NoError(t, err)

	require.NotEmpty(t, backend.seen, "the backend must have been asked to record something")
	for _, key := range backend.seen {
		assert.NotContains(t, key, "super-secret-payload", "the raw link payload must never be persisted")
		assert.NotContains(t, key, "super-secret-signature", "the link signature must never be persisted")
		assert.NotEqual(t, link, key)
	}
	for k := range backend.entries {
		assert.False(t, strings.Contains(k, "super-secret"), "no persisted key may embed the raw link")
	}
}

// TestConsumedLinkStore_BackendErrorFailsClosed: a storage failure must never
// be reported as a successful first use — that is precisely the replay the
// store exists to stop. It returns an error so the caller can surface an
// honest, retryable "couldn't verify this link" rather than the misleading
// "already used".
func TestConsumedLinkStore_BackendErrorFailsClosed(t *testing.T) {
	backend := newFakeConsumedBackend()
	backend.err = errors.New("apiserver unavailable")
	s := newConsumedLinkStore(35*time.Minute, backend)

	ok, err := s.markConsumed(context.Background(), "a.b")

	require.Error(t, err, "a storage failure must be surfaced, never swallowed")
	assert.False(t, ok, "a storage failure must not be reported as a successful first use")
}

// TestConsumedLinkStore_NoBackendIsMemoryOnly keeps the degraded path explicit:
// with no durable backend wired (unit tests, a dev server without K8s) the
// store still enforces single-use within the process, exactly as before.
func TestConsumedLinkStore_NoBackendIsMemoryOnly(t *testing.T) {
	s := newConsumedLinkStore(10*time.Minute, nil)
	ok, err := s.markConsumed(context.Background(), "a.b")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.markConsumed(context.Background(), "a.b")
	require.NoError(t, err)
	assert.False(t, ok, "single-use still holds in-process without a backend")
}
