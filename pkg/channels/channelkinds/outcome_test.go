package channelkinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every declared Outcome must have a distinct, non-placeholder wire label and
// must round-trip. This is the guard that fires when a new Outcome member is
// added without extending outcomeNames.
//
// The bound is outcomeSentinel, not the last-named member: with a hardcoded
// `o <= OutcomeRefused` this test silently skipped any newly appended member —
// the very thing it exists to catch — and that member crossed the wire as
// "unknown".
func TestEveryOutcomeHasADistinctLabelAndRoundTrips(t *testing.T) {
	seen := map[string]Outcome{}
	for o := OutcomeUnknown; o < outcomeSentinel; o++ {
		label := o.String()
		require.NotEmpty(t, label, "Outcome(%d) has no wire label", int(o))
		if o != OutcomeUnknown {
			require.NotEqual(t, "unknown", label,
				"Outcome(%d) fell through to the placeholder label — add it to outcomeNames", int(o))
		}
		if prev, dup := seen[label]; dup {
			t.Fatalf("Outcome(%d) and Outcome(%d) share the wire label %q", int(prev), int(o), label)
		}
		seen[label] = o

		got, ok := ParseOutcome(label)
		require.True(t, ok, "ParseOutcome(%q) must recognize its own String()", label)
		assert.Equal(t, o, got)
	}
}

func TestParseOutcomeFailsClosedOnUnknownLabel(t *testing.T) {
	cases := []string{"", "ROUTED", "routed ", "allow", "\x01", "denied"}
	for _, s := range cases {
		t.Run("unrecognized label "+s+" yields OutcomeUnknown,false", func(t *testing.T) {
			got, ok := ParseOutcome(s)
			assert.False(t, ok, "an unrecognized label must not be silently accepted")
			assert.Equal(t, OutcomeUnknown, got, "and must never guess a real outcome")
		})
	}
}

// The sentinel bounds the enum; it is not itself an outcome. It must never
// acquire a wire label, or a bogus "outcome" would become representable.
//
// The len check is a second, self-maintaining net: outcomeNames must hold
// exactly one label per declared member ([0, outcomeSentinel)). It stays green
// when a member is added *with* a label, and fails both when a member is added
// without one and when a label is added for a value outside the enum. Note the
// residual risk it cannot cover: a member declared *after* outcomeSentinel is
// invisible to every runtime check — hence the "ALWAYS KEEP IT LAST" comment on
// the const block.
func TestOutcomeSentinelIsNotAnOutcome(t *testing.T) {
	assert.Equal(t, "unknown", outcomeSentinel.String(),
		"outcomeSentinel must not have a wire label — it is a bound, not an outcome")
	_, ok := ParseOutcome("outcomeSentinel")
	assert.False(t, ok, "the sentinel's identifier must not parse as an outcome")

	assert.Len(t, outcomeNames, int(outcomeSentinel),
		"outcomeNames must have exactly one label per declared Outcome — a new member needs a label, "+
			"and no label may exist for a value outside the enum")
}

func TestOutcomeStringPinsTheWireContract(t *testing.T) {
	// These strings cross NATS and are asserted by channelsd's tests. Renaming
	// one is a wire-breaking change; this test makes that explicit.
	assert.Equal(t, "routed", OutcomeRouted.String())
	assert.Equal(t, "internal_error", OutcomeInternalError.String())
	assert.Equal(t, "denied_by_permission", OutcomeDeniedByPermission.String())
	assert.Equal(t, "no_active_session", OutcomeNoActiveSession.String())
	assert.Equal(t, "fork_pending", OutcomeForkPending.String())
	assert.Equal(t, "refused", OutcomeRefused.String())
	assert.Equal(t, "unknown", Outcome(99).String())
}
