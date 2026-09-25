package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	// Every shipped kind, so the table below asserts against the real answers
	// rather than a stub's. This is an EXTERNAL test package for that reason:
	// each kind package imports the registry, so only a package nothing
	// imports can import them all back.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// wantDeliversToHuman is the expected answer for each shipped kind, with the
// reason it is that answer. The reason is the point: the value is one word in
// a file nobody re-reads, and a row that says only `false` gives a future
// reader nothing to check the kind's own comment against.
//
// The map is looked up BY the loop below, never iterated to drive it — see
// TestDeliversToHuman_ShippedKinds for why that direction matters.
var wantDeliversToHuman = map[string]struct {
	want bool
	why  string
}{
	"slack":   {true, "a thread or DM read by the people in it"},
	"browser": {true, "a browser tab someone is looking at"},
	"local":   {true, "the terminal pane the local operator is sitting in"},
	"fake":    {true, "stands in for a human-facing surface; e2e scenarios answer its cards"},
	"bento":   {false, "scheduler-driven input with no rendering surface"},
	"agent":   {false, "the far side is another AgentSession, not a person"},
	"github":  {false, "no rendering surface: Capabilities is nil and NewSender refuses, so the relay's lineage walk climbs to the paired output Channel"},
}

// TestDeliversToHuman_ShippedKinds pins each kind's answer. The answer decides
// whether a permission prompt about a session reaches a person or an agent, and
// it is one word in a file nobody re-reads — so the roster is asserted in one
// place where flipping a value shows up as a failing test rather than as a
// misrouted card.
//
// Driven by registry.All() rather than by the roster's own keys, so a kind that
// ships without a row arrives HERE as a failure instead of as an omission. That
// direction is not theoretical: the hardcoded six-row list this replaced never
// met the github kind, and kept passing.
//
// The reverse sweep at the end closes the other half — a row for a kind that no
// longer exists would otherwise sit here asserting nothing forever.
func TestDeliversToHuman_ShippedKinds(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds,
		"the kind registry must be populated by init — an empty registry would make every row below vacuous")

	for _, k := range kinds {
		t.Run(k.Name(), func(t *testing.T) {
			exp, ok := wantDeliversToHuman[k.Name()]
			require.Truef(t, ok,
				"kind %q is registered but has no row in wantDeliversToHuman; decide whether a "+
					"permission or credential prompt about one of its sessions reaches a PERSON, "+
					"and say why", k.Name())

			got, err := registry.DeliversToHuman(k.Name())
			require.NoError(t, err, "kind %q must be registered by its blank import", k.Name())
			assert.Equal(t, exp.want, got, "%s: %s", k.Name(), exp.why)
		})
	}

	for name := range wantDeliversToHuman {
		t.Run("roster row "+name+" still names a registered kind", func(t *testing.T) {
			_, ok := registry.Get(name)
			assert.Truef(t, ok,
				"wantDeliversToHuman has a row for %q, which no kind registers; "+
					"drop the row or restore the blank import", name)
		})
	}
}

func TestDeliversToHuman_UnregisteredKind_IsAnErrorNotAFailSafeFalse(t *testing.T) {
	got, err := registry.DeliversToHuman("no-such-kind")

	require.ErrorIs(t, err, registry.ErrUnknownKind,
		"an unregistered kind is a wiring bug and must be distinguishable from a real answer")
	assert.False(t, got, "the value must still be the closed answer, whatever the caller does with the error")
	assert.Contains(t, err.Error(), "no-such-kind", "the error must name the kind so an operator can find it")
}
