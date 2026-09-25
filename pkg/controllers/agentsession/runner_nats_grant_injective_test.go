package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/natstest"
)

// The per-session runner's principal name is the ONLY input to the reply
// inbox, on both sides of the wire: the minted SubAllow entry
// (apnats.InboxSubjectFor, controller.go) and the client's CustomInboxPrefix,
// re-derived from the JWT's own Name claim (pkg/platform/nats.buildOptions). Two
// distinct sessions that produce the same name therefore share one inbox root,
// and each one's minted JWT genuinely authorizes subscribing to the other's
// replies — read_thread_history and read_channel_history among them, the
// per-session channel content pkg/channels/channelsd/historyresp keeps apart by
// deriving the channel key from the request SUBJECT.
//
// The name must be injective over (namespace, name), or refuse to be built at
// all. It must never silently fold two sessions together.
func TestRunnerNATSGrantNameIsInjectiveOverSessionCoordinates(t *testing.T) {
	// Every pair here is a distinct session. Several fold together under a
	// naive join: '-' is legal in both a DNS-1123 namespace and name, and '.'
	// is legal in a name and collapses to '-' in a NATS subject token.
	pairs := []struct{ ns, name string }{
		{"team-a", "bot"},
		{"team", "a-bot"},
		{"team", "a"},
		{"team", "bot"},
		{"team-a-bot", "x"},
	}

	seen := map[string]string{} // inbox prefix -> the session that claimed it
	for _, p := range pairs {
		g, err := runnerNATSUserGrant(p.ns, p.name)
		require.NoError(t, err, "grant for %s/%s", p.ns, p.name)
		prefix := apnats.InboxPrefixFor(g.Name)
		coords := p.ns + "/" + p.name
		if prior, dup := seen[prefix]; dup {
			t.Errorf("sessions %s and %s share reply inbox %q: each one's JWT authorizes reading the other's replies",
				prior, coords, prefix)
			continue
		}
		seen[prefix] = coords
	}
}

// A session name may legally contain a '.', which the inbox token map folds to
// '-' — so ("t", "a.b") and ("t", "a-b") would share an inbox under ANY
// delimiter. No separator can undo a lossy transform downstream of it, so the
// grant is refused outright rather than built and quietly folded. The same
// refusal covers the dot-joined subject prefix, where a session named "a" would
// otherwise hold a subtree grant over session "a.b".
func TestRunnerNATSUserGrantRefusesNamesThatCannotBeKeptDistinct(t *testing.T) {
	cases := []struct{ name, ns, sess string }{
		{name: "dotted session name: refused, it folds to '-' in the inbox token", ns: "team", sess: "a.b"},
		{name: "dotted namespace: refused for the same reason", ns: "te.am", sess: "bot"},
		{name: "underscore in the name: refused, it is the delimiter", ns: "team", sess: "a_bot"},
		{name: "empty namespace: refused rather than joined as nothing", ns: "", sess: "bot"},
		{name: "empty session name: refused rather than joined as nothing", ns: "team", sess: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runnerNATSUserGrant(tc.ns, tc.sess)
			require.Error(t, err, "grant for %q/%q must be refused, not folded", tc.ns, tc.sess)
			assert.Contains(t, err.Error(), "principal name",
				"the error must name what could not be built")
		})
	}
}

// The property above, settled by the server rather than by string comparison:
// mint both sessions' real grants and ask a real nats-server whether session B
// may subscribe to session A's inbox. A grant assertion that disagrees with the
// server is worse than none (see pkg/platform/nats/natstest), and this is precisely the
// subscription an inbox collision hands out.
func TestRunnerNATSGrantCannotSubscribeToAnotherSessionsInbox(t *testing.T) {
	victim, err := runnerNATSUserGrant("team-a", "bot")
	require.NoError(t, err, "grant for team-a/bot")
	attacker, err := runnerNATSUserGrant("team", "a-bot")
	require.NoError(t, err, "grant for team/a-bot")

	// A reply subject nats.go would actually generate for the victim:
	// "<prefix>.<nuid>.<token>".
	victimReply := apnats.InboxPrefixFor(victim.Name) + ".nuidnuidnuidnuidnuidnu.1"

	h := natstest.New(t)
	require.True(t, h.SubscribeAllowed(t, victim, victimReply),
		"precondition: a session must be able to subscribe to its OWN reply inbox")
	assert.False(t, h.SubscribeAllowed(t, attacker, victimReply),
		"session team/a-bot must NOT be authorized on session team-a/bot's reply inbox")
}
