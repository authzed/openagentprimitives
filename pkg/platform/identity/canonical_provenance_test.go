package identity_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// CanonicalUserID is the id that becomes a SpiceDB `user:` object, so whether
// it may be trusted depends entirely on where its bytes came from. As a
// defined string type it recorded nothing about that: 26 non-test sites
// converted a bare string straight into one, and several of those strings were
// the very inputs this audit found being trusted without verification — an
// unsigned X-Admin-Subject header, a self-asserted app-tool Requester, an
// email from a directory this deployment does not govern.
//
// It is now a struct with an unexported field, so the only ways in are named
// constructors that say where the bytes came from. That is the same shape
// plangate.Handle and memory.Approval already use, and it is why a bare
// conversion from a string no longer compiles.
func TestCanonical_VerifiedPathProducesAnID(t *testing.T) {
	got, err := identity.VerifiedEmail("Alice@Corp.example", "Alice").Canonical()

	require.NoError(t, err)
	assert.NotEmpty(t, got.String(), "a verified principal must canonicalize")
}

// The escape hatch exists because not every site can verify today. It names
// itself and REQUIRES a stated reason, so `grep CanonicalFromTrusted`
// enumerates every place the platform takes someone's word for an identity.
func TestCanonical_TrustedEscapeHatchProducesTheID(t *testing.T) {
	got := identity.CanonicalFromTrusted("alice", "operator-written annotation")

	assert.Equal(t, "alice", got.String())
}

// The zero value is empty, so a forgotten assignment fails closed rather than
// reading as a valid identity.
func TestCanonical_ZeroValueIsEmpty(t *testing.T) {
	var c identity.CanonicalUserID

	assert.Empty(t, c.String())
	assert.True(t, c.IsZero())
}

// It still round-trips through JSON, because one wire type carries it.
func TestCanonical_JSONRoundTripsAsAString(t *testing.T) {
	orig, err := identity.VerifiedEmail("alice@corp.example", "Alice").Canonical()
	require.NoError(t, err)

	raw, err := json.Marshal(orig)
	require.NoError(t, err)
	assert.JSONEq(t, `"`+orig.String()+`"`, string(raw), "the wire form is just the id")

	var back identity.CanonicalUserID
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, orig, back)
}

// EQUALITY IS IDENTITY, and this is the property the first cut of this type got
// wrong — caught by the suite, not by review.
//
// Provenance stored as a field participates in Go's struct ==, so the same
// person reached by two paths compared unequal. This type is used as a map
// key, so that is a silent-miss landmine strictly worse than the problem the
// type exists to solve. Carrying verified-ness to a sink needs a SECOND type
// the sink can demand, not a field on this one.
func TestCanonical_EqualityIsIdentityRegardlessOfHowItWasObtained(t *testing.T) {
	verified, err := identity.VerifiedEmail("alice@corp.example", "Alice").Canonical()
	require.NoError(t, err)
	trusted := identity.CanonicalFromTrusted(verified.String(), "annotation")

	assert.Equal(t, verified, trusted,
		"the same person reached by two paths must be the same value")

	m := map[identity.CanonicalUserID]string{verified: "x"}
	assert.Equal(t, "x", m[trusted],
		"a map keyed on a canonical must hit whichever path produced the key")
}
