package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These literals are the canonical encodings existing SpiceDB
// relationships were written with. They must NEVER change — the
// Principal refactor (and any future one) must reproduce them
// byte-identically or existing started_by/share relationships orphan.
//
// The synthetic rows carry AllowSynthetic(): the encoding is unchanged, but
// minting it now requires the explicit opt-in (else Canonical errors).
func TestCanonicalGoldenVectors(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want string
	}{
		{"email basis", EmailReference(Email("alice@example.com")), "YWxpY2VAZXhhbXBsZS5jb20"},
		{"email lowercased", EmailReference(Email("Alice@Example.COM")), "YWxpY2VAZXhhbXBsZS5jb20"},
		{"email wins over synthetic fields", FromExternal(KindSlack, TeamScope("T123"), RawExternalID("U456"), Email("alice@example.com")), "YWxpY2VAZXhhbXBsZS5jb20"},
		{"slack synthetic", FromExternal(KindSlack, TeamScope("T123"), RawExternalID("U456"), Email("")).AllowSynthetic(), "c2xhY2s6VDEyMzpVNDU2"},
		{"local synthetic (empty team)", FromExternal(KindLocal, TeamScope(""), RawExternalID("alice"), Email("")).AllowSynthetic(), "bG9jYWw6OmFsaWNl"},
		{"cli synthetic", FromExternal(Kind("cli"), TeamScope(""), RawExternalID("carol"), Email("")).AllowSynthetic(), "Y2xpOjpjYXJvbA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.p.Canonical()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
			// CanonicalUserID.Subject() must reproduce the same "user:"+bytes every
			// wire-DTO producer's hand-built `"user:"+canon.String()` used to yield.
			assert.Equal(t, Subject("user:"+tc.want), got.Subject())
		})
	}
}

// TestCanonical_RefusesSyntheticWithoutOptIn is the fail-closed guard: an
// email-less principal that did NOT opt into the synthetic encoding must
// return ErrSyntheticSubject from both Canonical and Subject, and must
// succeed (with the stable synthetic encoding) once AllowSynthetic is set.
// This is what turns "dropped email → phantom subject → owner approval
// mis-handled" into a loud, caught error at the source.
func TestCanonical_RefusesSyntheticWithoutOptIn(t *testing.T) {
	emailless := FromExternal(KindSlack, TeamScope("T1"), RawExternalID("U1"), Email(""))

	t.Run("Canonical errors without opt-in", func(t *testing.T) {
		_, err := emailless.Canonical()
		require.ErrorIs(t, err, ErrSyntheticSubject)
	})
	t.Run("Subject errors without opt-in", func(t *testing.T) {
		_, err := emailless.Subject()
		require.ErrorIs(t, err, ErrSyntheticSubject)
	})
	t.Run("AllowSynthetic mints the stable synthetic encoding", func(t *testing.T) {
		canon, err := emailless.AllowSynthetic().Canonical()
		require.NoError(t, err)
		assert.Equal(t, "c2xhY2s6VDE6VTE", canon.String())
		sub, err := emailless.AllowSynthetic().Subject()
		require.NoError(t, err)
		assert.Equal(t, "user:c2xhY2s6VDE6VTE", sub.String())
	})
	t.Run("email-bearing principal never errors and ignores AllowSynthetic", func(t *testing.T) {
		p := FromExternal(KindSlack, TeamScope("T1"), RawExternalID("U1"), Email("alice@example.com"))
		got, err := p.Canonical()
		require.NoError(t, err)
		gotAllow, err := p.AllowSynthetic().Canonical()
		require.NoError(t, err)
		assert.Equal(t, got, gotAllow, "email wins the canonical; AllowSynthetic is a no-op")
	})
	t.Run("RawSubject never errors", func(t *testing.T) {
		sub, err := RawSubject("user:abc").Subject()
		require.NoError(t, err)
		assert.Equal(t, "user:abc", sub.String())
	})
}
