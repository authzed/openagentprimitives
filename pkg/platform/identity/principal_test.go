package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrincipalCanonicalMatchesGoldenVectors(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		want string
	}{
		{"VerifiedEmail", VerifiedEmail(Email("Alice@Example.COM"), "Alice"), "YWxpY2VAZXhhbXBsZS5jb20"},
		{"EmailReference", EmailReference(Email("alice@example.com")), "YWxpY2VAZXhhbXBsZS5jb20"},
		{"FromExternal email wins", FromExternal(KindSlack, TeamScope("T123"), RawExternalID("U456"), Email("alice@example.com")), "YWxpY2VAZXhhbXBsZS5jb20"},
		{"FromExternal synthetic", FromExternal(KindSlack, TeamScope("T123"), RawExternalID("U456"), Email("")).AllowSynthetic(), "c2xhY2s6VDEyMzpVNDU2"},
		{"FromExternal local", FromExternal(KindLocal, TeamScope(""), RawExternalID("alice"), Email("")).AllowSynthetic(), "bG9jYWw6OmFsaWNl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canon, err := tc.p.Canonical()
			require.NoError(t, err)
			assert.Equal(t, tc.want, canon.String())
			sub, err := tc.p.Subject()
			require.NoError(t, err)
			assert.Equal(t, "user:"+tc.want, sub.String())
		})
	}
}

func TestPrincipalVerifiedness(t *testing.T) {
	assert.True(t, VerifiedEmail(Email("a@b.co"), "").EmailVerified())
	assert.False(t, EmailReference(Email("a@b.co")).EmailVerified())
	assert.True(t, FromExternal(KindSlack, TeamScope("T"), RawExternalID("U"), Email("a@b.co")).EmailVerified(),
		"FromExternal email is channel-verified by contract")
	assert.False(t, FromExternal(KindLocal, TeamScope(""), RawExternalID("u"), Email("")).EmailVerified())
	assert.False(t, IdPUser(Email("a@b.co"), false, "").EmailVerified())
	assert.True(t, IdPUser(Email("a@b.co"), true, "").EmailVerified())
}

func TestRawSubjectPassthrough(t *testing.T) {
	p := RawSubject("user:abc123")
	sub, err := p.Subject()
	require.NoError(t, err)
	assert.Equal(t, "user:abc123", sub.String())
	canon, err := p.Canonical()
	require.NoError(t, err)
	assert.Equal(t, "abc123", canon.String())
	assert.False(t, p.EmailVerified())
}
