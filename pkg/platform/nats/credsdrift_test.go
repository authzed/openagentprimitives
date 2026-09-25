package nats_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
)

// mintedFor mints a real creds file for g against a throwaway identity, so
// every case below compares against an actual signed JWT rather than a
// hand-built claims struct.
func mintedFor(t *testing.T, g apnats.UserGrant) string {
	t.Helper()
	id, err := apnats.GenerateIdentity()
	require.NoError(t, err, "generate identity")
	creds, err := apnats.MintUser(id, g)
	require.NoError(t, err, "mint %q", g.Name)
	return creds
}

func TestUserGrant_CredsDrift(t *testing.T) {
	shipped := apnats.UserGrant{
		Name:     "webd",
		PubAllow: []string{"ap.session.*.*.in.view_message", "ap.channel.webhook_inbound"},
		SubAllow: []string{"ap.session.*.*.out.>", apnats.InboxSubjectFor("webd")},
	}

	cases := []struct {
		name  string
		creds func(t *testing.T) string
		// want is a substring of the reported reason; "" means "no drift".
		want string
	}{
		{
			name:  "creds minted from the shipped grant: no drift",
			creds: func(t *testing.T) string { return mintedFor(t, shipped) },
		},
		{
			name: "same subjects in a different order: no drift, so no needless rotation",
			creds: func(t *testing.T) string {
				return mintedFor(t, apnats.UserGrant{
					Name:     shipped.Name,
					PubAllow: []string{"ap.channel.webhook_inbound", "ap.session.*.*.in.view_message"},
					SubAllow: []string{apnats.InboxSubjectFor("webd"), "ap.session.*.*.out.>"},
				})
			},
		},
		{
			name: "the upgrade case: a JWT minted before a publish subject was added",
			creds: func(t *testing.T) string {
				return mintedFor(t, apnats.UserGrant{
					Name:     shipped.Name,
					PubAllow: []string{"ap.session.*.*.in.view_message"},
					SubAllow: shipped.SubAllow,
				})
			},
			want: "publish allow-list is missing ap.channel.webhook_inbound",
		},
		{
			name: "a withdrawn subject still in the stored JWT",
			creds: func(t *testing.T) string {
				return mintedFor(t, apnats.UserGrant{
					Name:     shipped.Name,
					PubAllow: append([]string{"ap.>"}, shipped.PubAllow...),
					SubAllow: shipped.SubAllow,
				})
			},
			want: "publish allow-list still carries withdrawn ap.>",
		},
		{
			name: "a subscribe subject missing",
			creds: func(t *testing.T) string {
				return mintedFor(t, apnats.UserGrant{
					Name:     shipped.Name,
					PubAllow: shipped.PubAllow,
					SubAllow: []string{"ap.session.*.*.out.>"},
				})
			},
			want: "subscribe allow-list is missing " + apnats.InboxSubjectFor("webd"),
		},
		{
			name: "a JWT minted for a different principal",
			creds: func(t *testing.T) string {
				return mintedFor(t, apnats.UserGrant{
					Name: "channelsd", PubAllow: shipped.PubAllow, SubAllow: shipped.SubAllow,
				})
			},
			want: `named "channelsd", not "webd"`,
		},
		{
			name:  "nothing stored at all",
			creds: func(*testing.T) string { return "" },
			want:  "no user JWT is stored",
		},
		{
			// ParseDecoratedJWT hands undecorated text straight through, so
			// this lands in the decode arm rather than the parse arm. Either
			// way it must read as drift and name why, never as "in sync".
			name:  "an unparseable creds file",
			creds: func(*testing.T) string { return "this is not a creds file" },
			want:  "could not be decoded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shipped.CredsDrift(tc.creds(t))
			if tc.want == "" {
				assert.Empty(t, got, "no rotation should be triggered")
				return
			}
			assert.Contains(t, got, tc.want,
				"the reason is what the installer prints; it must name the actual difference")
		})
	}
}

// TestGrantFromCreds_RoundTripsWhatWasMinted pins the inverse relationship
// CredsDrift depends on: what MintUser writes into the JWT is what
// GrantFromCreds reads back.
func TestGrantFromCreds_RoundTripsWhatWasMinted(t *testing.T) {
	want := apnats.UserGrant{
		Name:     "cli",
		PubAllow: []string{"ap.>", "_INBOX.>"},
		SubAllow: []string{"ap.>", apnats.InboxSubjectFor("cli")},
	}
	got, err := apnats.GrantFromCreds(mintedFor(t, want))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestGrantFromCreds_RefusesGarbage(t *testing.T) {
	_, err := apnats.GrantFromCreds("   ")
	require.Error(t, err)

	_, err = apnats.GrantFromCreds(strings.Repeat("x", 64))
	require.Error(t, err)
}
