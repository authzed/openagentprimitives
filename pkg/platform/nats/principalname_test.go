package nats

import (
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PrincipalName's contract is a property, not a format: distinct part lists
// must never produce one name, because that name is the whole of a principal's
// reply-inbox identity. The pairs below are the ones a naive join folds
// together — '-' is legal in both a DNS-1123 namespace and name, and '.' is
// legal in a name and collapses to '-' inside a NATS subject token.
func TestPrincipalNameIsInjectiveOverItsParts(t *testing.T) {
	partLists := [][]string{
		{"runner", "team-a", "bot"},
		{"runner", "team", "a-bot"},
		{"runner", "team", "a"},
		{"runner", "team-a-bot", "x"},
		{"runner", "team", "bot"},
		{"runner", "team", "botx"},
	}

	seen := map[string][]string{} // inbox prefix -> the parts that claimed it
	for _, parts := range partLists {
		name, err := PrincipalName(parts...)
		require.NoError(t, err, "PrincipalName(%q)", parts)

		// Compare the INBOX PREFIX, not the name: the prefix is what both sides
		// of the wire actually agree on, and sanitizeInboxToken sits between
		// the two. Two distinct names that sanitize to one prefix would be the
		// same leak.
		prefix := InboxPrefixFor(name)
		if prior, dup := seen[prefix]; dup {
			t.Errorf("parts %q and %q both yield inbox %q", prior, parts, prefix)
			continue
		}
		seen[prefix] = parts
	}
}

// The refusal is the mechanism. Sanitizing an unsafe part and carrying on would
// hand back a name that looks fine and silently shares an inbox with another
// principal; there is no later point at which that can be noticed, because both
// sides derive the same string and nothing disagrees.
func TestPrincipalNameRefusesPartsThatBreakInjectivity(t *testing.T) {
	cases := []struct {
		name    string
		parts   []string
		wantErr string
	}{
		{
			name:    "part contains the delimiter: refused, the split would be ambiguous",
			parts:   []string{"runner", "team", "a_bot"},
			wantErr: `"_"`,
		},
		{
			name:    "part contains a dot: refused, it folds to '-' in the inbox token",
			parts:   []string{"runner", "team", "a.bot"},
			wantErr: `"."`,
		},
		{
			name:    "part contains a slash: refused, no delimiter survives the fold",
			parts:   []string{"runner", "team/bot"},
			wantErr: `"/"`,
		},
		{
			name:    "empty part: refused rather than joined as nothing",
			parts:   []string{"runner", "", "bot"},
			wantErr: "is empty",
		},
		{
			name:    "no parts at all: refused",
			parts:   nil,
			wantErr: "at least one part",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PrincipalName(tc.parts...)
			require.Error(t, err, "PrincipalName(%q) must be refused", tc.parts)
			assert.Empty(t, got, "a refused name must not also be returned")
			assert.Contains(t, err.Error(), tc.wantErr,
				"the error must name what is wrong with the input")
		})
	}
}

// A composed name has to survive everything between construction and the wire,
// or the fix trades a leak for a total loss of request/reply.
func TestPrincipalNameRoundTripsToAUsableInboxPrefix(t *testing.T) {
	name, err := PrincipalName("runner", "default", "messaging-bot-channel-8cf75632")
	require.NoError(t, err)

	assert.Equal(t, InboxRoot+"."+name, InboxPrefixFor(name),
		"sanitizeInboxToken must be the identity on a PrincipalName, so the mint-time "+
			"grant and the client's re-derivation from the JWT cannot diverge")
	require.NoError(t, natsgo.CustomInboxPrefix(InboxPrefixFor(name))(&natsgo.Options{}),
		"nats.go must accept the derived prefix, or every connection fails at dial")
	assert.Equal(t, InboxPrefixFor(name)+".>", InboxSubjectFor(name),
		"the SubAllow entry must cover <prefix>.<nuid>.<token>")
}
