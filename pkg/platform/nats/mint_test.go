package nats

import (
	"testing"

	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateIdentityProducesValidChain(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err, "GenerateIdentity")

	acc, err := jwt.DecodeAccountClaims(id.AccountJWT)
	require.NoError(t, err, "account JWT must decode")
	assert.Contains(t, acc.SigningKeys, id.AccountSigningPublicKey,
		"account JWT must list the signing key")
}

func TestMintUserScopesSubjects(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err)

	creds, err := MintUser(id, UserGrant{
		Name:     "runner-ns1-sess1",
		PubAllow: []string{"ap.session.ns1.sess1.>"},
		SubAllow: []string{"ap.session.ns1.sess1.>"},
	})
	require.NoError(t, err, "MintUser")
	assert.Contains(t, creds, "BEGIN NATS USER JWT", "creds file must be in NATS creds format")

	ujwt, err := jwt.ParseDecoratedJWT([]byte(creds))
	require.NoError(t, err, "parse creds")
	uc, err := jwt.DecodeUserClaims(ujwt)
	require.NoError(t, err, "decode user claims")
	assert.Equal(t, []string{"ap.session.ns1.sess1.>"}, []string(uc.Pub.Allow))
	assert.Equal(t, []string{"ap.session.ns1.sess1.>"}, []string(uc.Sub.Allow))
	assert.Equal(t, id.AccountPublicKey, uc.IssuerAccount,
		"user JWT must name the account as issuer")
}

// TestMintUserEmptyAllowListsStayEmptyInTheJWT pins what MintUser does with an
// empty allow-list: it emits an empty list rather than synthesizing ">".
//
// It does NOT establish that such a user cannot publish, and must not be named
// as though it did: a nats-server builds a publish permission only when the
// allow or deny list is non-empty, so an empty list leaves the user able to
// publish anywhere. A test asserting a JWT field cannot speak for the server's
// enforcement — see natstest for the assertions that can.
func TestMintUserEmptyAllowListsStayEmptyInTheJWT(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err)

	creds, err := MintUser(id, UserGrant{
		Name:     "empty-allow-list-user",
		PubAllow: nil,
		SubAllow: nil,
	})
	require.NoError(t, err, "MintUser with empty allow-lists")

	ujwt, err := jwt.ParseDecoratedJWT([]byte(creds))
	require.NoError(t, err, "parse creds")
	uc, err := jwt.DecodeUserClaims(ujwt)
	require.NoError(t, err, "decode user claims")

	assert.Empty(t, uc.Pub.Allow, "empty PubAllow must not be converted to allow-all in the JWT")
	assert.Empty(t, uc.Sub.Allow, "empty SubAllow must not be converted to allow-all in the JWT")
}

// The inbox prefix is the one string a grant and its client must agree on, so
// it has to be a total function of the principal's name — including for names
// carrying characters that are not valid inside a single NATS subject token.
func TestInboxPrefixForIsOneSanitizedToken(t *testing.T) {
	cases := []struct {
		name       string
		principal  string
		wantPrefix string
	}{
		{
			name:       "plain principal: prefix is root plus the name",
			principal:  "webd",
			wantPrefix: "_INBOX.webd",
		},
		{
			name:       "per-session runner name: hyphens survive, still one token",
			principal:  "runner-default-messaging-bot-channel-8cf75632",
			wantPrefix: "_INBOX.runner-default-messaging-bot-channel-8cf75632",
		},
		{
			name:       "dots would add subject tokens: replaced",
			principal:  "runner-ap.session.default.sess1",
			wantPrefix: "_INBOX.runner-ap-session-default-sess1",
		},
		{
			name:       "wildcards would make the prefix illegal: replaced",
			principal:  "weird>name*here",
			wantPrefix: "_INBOX.weird-name-here",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantPrefix, InboxPrefixFor(tc.principal))
			assert.Equal(t, tc.wantPrefix+".>", InboxSubjectFor(tc.principal),
				"SubAllow entry must be the prefix plus '>' — nats.go replies are <prefix>.<nuid>.<token>")
			// A prefix nats.go itself refuses would fail at connect time, in a
			// place with nothing to point back here.
			require.NoError(t, natsgo.CustomInboxPrefix(InboxPrefixFor(tc.principal))(&natsgo.Options{}),
				"nats.go must accept the derived prefix")
		})
	}
}

// An unnamed grant cannot have its inbox scoped on either side, so minting one
// is refused rather than quietly producing a user that falls back to the shared
// inbox root.
func TestMintUserRejectsUnnamedGrant(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err)

	for _, name := range []string{"", "   "} {
		_, err := MintUser(id, UserGrant{Name: name, PubAllow: []string{"ap.>"}})
		require.Error(t, err, "MintUser(%q) must be refused", name)
		assert.Contains(t, err.Error(), "Name is required")
	}
}
