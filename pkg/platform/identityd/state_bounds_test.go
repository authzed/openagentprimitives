package identityd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every anonymous hit on /oidc/login inserts an entry that lives for the
// 10-minute TTL, and the link needed to reach it is handed out by a
// cookie-less GET on /admin and is not single-use for that window. With no cap
// the only bound on this map is how fast an attacker can issue requests
// against a pod capped at 256Mi.
//
// gcLocked evicts on TTL alone, which does not help: the whole attack fits
// inside one TTL window.
func TestLinkStateStore_BoundsTheNumberOfEntries(t *testing.T) {
	s := newLinkStateStore()

	for i := 0; i < maxLinkStates+500; i++ {
		_, err := s.NewState("link-"+strings.Repeat("x", 8), "b", "idp")
		require.NoError(t, err)
	}

	assert.LessOrEqual(t, s.len(), maxLinkStates,
		"an unbounded store is an unauthenticated OOM of the shared webd pod")
}

// Eviction must take the OLDEST entries, so a flood cannot push out a
// legitimate in-flight login that started moments earlier than the flood.
func TestLinkStateStore_EvictsOldestFirst(t *testing.T) {
	s := newLinkStateStore()

	// The newest token minted before the cap is reached must survive a flood
	// that arrives after it.
	var newest string
	for i := 0; i < maxLinkStates; i++ {
		tok, err := s.NewState("early", "b", "idp")
		require.NoError(t, err)
		newest = tok
	}
	for i := 0; i < 10; i++ {
		_, err := s.NewState("flood", "b", "idp")
		require.NoError(t, err)
	}

	_, _, refusal := s.Consume(newest, "b", "idp")
	assert.Equal(t, stateAccepted, refusal, "the most recent legitimate state must outlive an older-entry eviction")
}

// `next` is caller-supplied and was stored before any validation — safeNext
// runs only at redirect time — so a megabyte of query string was retained per
// request. It is bounded at the door: anything this long is not a path.
func TestLinkStateStore_RefusesAnOverlongNext(t *testing.T) {
	s := newLinkStateStore()

	_, err := s.NewStateWithNext("link", "/"+strings.Repeat("a", maxNextLen), "b", "idp")
	require.Error(t, err, "an over-long next must be refused rather than retained for the TTL")
}

// A next of ordinary length is the common case and must be stored verbatim,
// since the callback redirects to it.
func TestLinkStateStore_KeepsAnOrdinaryNext(t *testing.T) {
	s := newLinkStateStore()

	tok, err := s.NewStateWithNext("link", "/admin", "b", "idp")
	require.NoError(t, err)

	_, next, refusal := s.Consume(tok, "b", "idp")
	require.Equal(t, stateAccepted, refusal)
	assert.Equal(t, "/admin", next)
}
