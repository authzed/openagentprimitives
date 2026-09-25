package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	// Every shipped kind, so the table below asserts against the real answers
	// rather than a stub's — same reason deliverstohuman_test.go imports them
	// all, and why this is an EXTERNAL test package.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// wantAllowsSessionCounterparty is the expected answer for each shipped kind,
// with the reason it is that answer. It is looked up BY the loop below, never
// iterated to drive it — see TestAllowsSessionCounterparty_ShippedKinds.
var wantAllowsSessionCounterparty = map[string]struct {
	want bool
	why  string
}{
	"agent":   {true, "the counterparty IS another AgentSession; that is the whole kind"},
	"slack":   {false, "a thread or DM read by the people in it"},
	"browser": {false, "a browser tab someone is looking at"},
	"local":   {false, "the terminal pane the local operator is sitting in"},
	"fake":    {false, "stands in for a human-facing surface"},
	"bento":   {false, "scheduler-driven input; there is no session on the far side"},
	"github":  {false, "a pull request, not a session; its authzSubject names a service, never an agentsession:"},
}

// TestAllowsSessionCounterparty_ShippedKinds pins each kind's answer. Two
// separate decisions read it — whether a delegated child is offered ask_parent,
// and whether a session is offered respond_to_user — and both change meaning if
// a value here flips, so the roster is asserted in one place where a flip shows
// up as a failing test rather than as a tool quietly appearing or vanishing.
//
// A third reads it indirectly: the answer is what admits `agentsession:` into
// pipeline's authzSubjectAllowedTypes, so a kind flipped to true could carry a
// Channel whose declared counterparty is a session.
//
// Driven by registry.All() rather than by the roster's own keys, so a kind that
// ships without a row arrives HERE as a failure instead of as an omission — the
// hardcoded six-row list this replaced never met the github kind and kept
// passing. The reverse sweep at the end catches a row whose kind is gone.
func TestAllowsSessionCounterparty_ShippedKinds(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds,
		"the kind registry must be populated by init — an empty registry would make every row below vacuous")

	for _, k := range kinds {
		t.Run(k.Name(), func(t *testing.T) {
			exp, ok := wantAllowsSessionCounterparty[k.Name()]
			require.Truef(t, ok,
				"kind %q is registered but has no row in wantAllowsSessionCounterparty; decide "+
					"whether one of its Channels may declare an agentsession: counterparty, and say why",
				k.Name())

			got, err := registry.AllowsSessionCounterparty(k.Name())
			require.NoError(t, err, "kind %q must be registered by its blank import", k.Name())
			assert.Equal(t, exp.want, got, "%s: %s", k.Name(), exp.why)
		})
	}

	for name := range wantAllowsSessionCounterparty {
		t.Run("roster row "+name+" still names a registered kind", func(t *testing.T) {
			_, ok := registry.Get(name)
			assert.Truef(t, ok,
				"wantAllowsSessionCounterparty has a row for %q, which no kind registers; "+
					"drop the row or restore the blank import", name)
		})
	}
}

// An unregistered name is an ERROR rather than a fail-safe false, because the
// two consumers want OPPOSITE fail-safe answers and only an error lets each
// pick its own. A bool would be fail-safe for one and fail-OPEN for the other.
// TestFirstSessionCounterparty pins the derivation two independent decisions
// share: whether respond_to_user is offered at all, and whether the
// `artifact-delivered` completion requirement can be answered. What matters is
// that either binding reaching a session is enough, that the OUTBOUND one is
// the one reported when both do, and that an unresolvable kind names itself
// rather than coming back as a quiet "no".
func TestFirstSessionCounterparty(t *testing.T) {
	cases := []struct {
		name        string
		input, outb string
		wantRole    string
		wantKind    string
		wantReaches bool
		wantErrKind bool
	}{
		{
			name:  "both human-facing: no session counterparty, no error",
			input: "slack", outb: "slack",
		},
		{
			name:  "outbound reaches a session: reported as the outbound binding",
			input: "slack", outb: "agent",
			wantRole: "outbound", wantKind: "agent", wantReaches: true,
		},
		{
			// The mirror shape — input to a session, output to a person — is
			// withheld for no gain today and is deliberately still asked: it is
			// the direction that cannot re-open the laundering path.
			name:  "input reaches a session while outbound does not: still reported",
			input: "agent", outb: "slack",
			wantRole: "input", wantKind: "agent", wantReaches: true,
		},
		{
			name:  "both reach a session: the outbound binding is the one named",
			input: "agent", outb: "agent",
			wantRole: "outbound", wantKind: "agent", wantReaches: true,
		},
		{
			name:  "unregistered outbound kind: error naming that binding",
			input: "slack", outb: "no-such-kind",
			wantRole: "outbound", wantKind: "no-such-kind", wantErrKind: true,
		},
		{
			name:  "unregistered input kind: error naming that binding",
			input: "no-such-kind", outb: "slack",
			wantRole: "input", wantKind: "no-such-kind", wantErrKind: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, reaches, err := registry.FirstSessionCounterparty(tc.input, tc.outb)

			if tc.wantErrKind {
				require.ErrorIs(t, err, registry.ErrUnknownKind)
				assert.False(t, reaches, "an unresolved kind must never come back as a positive answer")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantReaches, reaches)
			}
			assert.Equal(t, tc.wantRole, b.Role, "the binding that decided must be named, so a caller can say which")
			assert.Equal(t, tc.wantKind, b.Kind)
		})
	}
}

func TestAllowsSessionCounterparty_UnregisteredNameIsAnError(t *testing.T) {
	for _, name := range []string{"no-such-kind", ""} {
		t.Run("name="+name, func(t *testing.T) {
			got, err := registry.AllowsSessionCounterparty(name)
			require.ErrorIs(t, err, registry.ErrUnknownKind)
			assert.False(t, got, "an unresolved kind must never come back as a positive answer")
		})
	}
}
