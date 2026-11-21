package identity_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// mustCanon canonicalizes p, failing the test on the ErrSyntheticSubject
// guard. Used where the principal provably carries an email or has opted into
// the synthetic encoding.
func mustCanon(t *testing.T, p identity.Principal) string {
	t.Helper()
	c, err := p.Canonical()
	require.NoError(t, err)
	return c.String()
}

func TestCanonicalize_EmailLowercased(t *testing.T) {
	a := mustCanon(t, identity.EmailReference(identity.Email("Alice@Example.com")))
	b := mustCanon(t, identity.EmailReference(identity.Email("alice@example.com")))
	assert.Equal(t, b, a, "email case must not change canonical")
	assert.NotEmpty(t, a, "non-empty input must produce non-empty canonical")
}

func TestCanonicalize_FallbackWhenEmailMissing(t *testing.T) {
	// The synthetic fallback is now opt-in: it requires AllowSynthetic.
	p := identity.FromExternal(identity.KindSlack, identity.TeamScope("T1"), identity.RawExternalID("U1"), identity.Email("")).AllowSynthetic()
	got := mustCanon(t, p)
	assert.NotEmpty(t, got, "fallback canonical must be non-empty")
	got2 := mustCanon(t, p)
	assert.Equal(t, got, got2, "canonical must be stable")
}

func TestCanonicalize_DifferentInputsDifferentOutputs(t *testing.T) {
	a := mustCanon(t, identity.EmailReference(identity.Email("a@x")))
	b := mustCanon(t, identity.EmailReference(identity.Email("b@x")))
	assert.NotEqual(t, a, b, "different emails must produce different canonicals")
}

func TestDecodeForDisplay_RoundtripsEmail(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    string
	}{
		{name: "bare canonical", subject: mustCanon(t, identity.EmailReference(identity.Email("alice@example.com"))), want: "alice@example.com"},
		{name: "user:-prefixed canonical", subject: "user:" + mustCanon(t, identity.EmailReference(identity.Email("alice@example.com"))), want: "alice@example.com"},
		{name: "synthetic fallback", subject: mustCanon(t, identity.FromExternal(identity.KindSlack, identity.TeamScope("T1"), identity.RawExternalID("U1"), identity.Email("")).AllowSynthetic()), want: "slack:T1:U1"},
		{name: "user:-prefixed synthetic", subject: "user:" + mustCanon(t, identity.FromExternal(identity.KindSlack, identity.TeamScope("T1"), identity.RawExternalID("U1"), identity.Email("")).AllowSynthetic()), want: "slack:T1:U1"},
		{name: "undecodable falls back to input", subject: "user:not-base64!!!", want: "user:not-base64!!!"},
		{name: "empty after prefix returns input", subject: "user:", want: "user:"},
		{name: "empty input", subject: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, identity.DecodeForDisplay(tc.subject))
		})
	}
}

func TestSplitSynthetic(t *testing.T) {
	cases := []struct {
		name     string
		decoded  string
		wantKind identity.Kind
		wantTeam identity.TeamScope
		wantID   identity.RawExternalID
		wantOK   bool
	}{
		{name: "full triple: all three parts", decoded: "slack:T1:U1", wantKind: "slack", wantTeam: "T1", wantID: "U1", wantOK: true},
		{name: "empty team scope: still a triple", decoded: "slack::U1", wantKind: "slack", wantID: "U1", wantOK: true},
		{name: "colon inside the external id: kept whole", decoded: "github:org:a:b", wantKind: "github", wantTeam: "org", wantID: "a:b", wantOK: true},
		{name: "two parts: not ok", decoded: "slack:U1", wantOK: false},
		{name: "blank external id: not ok", decoded: "slack:T1: ", wantOK: false},
		{name: "email payload: not ok", decoded: "alice@example.com", wantOK: false},
		{name: "empty: not ok", decoded: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, team, id, ok := identity.SplitSynthetic(tc.decoded)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantKind, kind)
			assert.Equal(t, tc.wantTeam, team)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

func TestIsCanonicalUserIDShape(t *testing.T) {
	syntheticSubject, err := identity.FromExternal(
		identity.KindSlack, identity.TeamScope("T1"), identity.RawExternalID("U1"), identity.Email(""),
	).AllowSynthetic().Subject()
	require.NoError(t, err)

	cases := []struct {
		name string
		s    string
		want bool
	}{
		{name: "empty string: not canonical-shaped", s: "", want: false},
		{name: "raw email: not canonical-shaped", s: "alice@example.com", want: false},
		{name: "invalid unpadded base64url length: not canonical-shaped", s: "alice", want: false},
		{
			name: "the RawURLEncoding of an email: canonical-shaped",
			s:    base64.RawURLEncoding.EncodeToString([]byte("alice@example.com")),
			want: true,
		},
		{name: "a short well-formed base64url id: canonical-shaped", s: "abc123", want: true},
		{
			name: "a synthetic canonical from FromExternal(...).AllowSynthetic(): canonical-shaped",
			s:    strings.TrimPrefix(string(syntheticSubject), "user:"),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, identity.IsCanonicalUserIDShape(tc.s))
		})
	}
}
