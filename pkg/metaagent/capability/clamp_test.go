package capability_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
)

// A capability moves WITHIN a bound an administrator already set; it never
// raises one. Clamping happens before the card renders, so a human is never
// shown — and can never grant — a value outside that bound.
func TestClampNumeric_boundsARequestToTheAdminCeiling(t *testing.T) {
	ceiling := capability.Bound{Permitted: true, Max: 500_000}

	cases := []struct {
		name      string
		requested int64
		want      int64
		clamped   bool
	}{
		{"above the ceiling is cut to it", 2_000_000, 500_000, true},
		{"at the ceiling passes untouched", 500_000, 500_000, false},
		{"below the ceiling passes untouched", 100_000, 100_000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, clamped, err := capability.ClampNumeric(tc.requested, ceiling)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.clamped, clamped)
		})
	}
}

// A request BELOW an administrative floor is refused, not silently raised.
//
// Raising it would grant more than was asked for, which turns a narrowing
// utterance into a widening outcome — the one direction that must never happen
// without approval. Refusing is honest and cannot surprise anyone.
func TestClampNumeric_aRequestBelowTheFloorIsRefusedNotRaised(t *testing.T) {
	_, _, err := capability.ClampNumeric(10, capability.Bound{Permitted: true, Min: 1000, Max: 5000})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "1000", "the message must state the floor the request missed")
}

// A capability an admin has switched off is refused regardless of standing —
// `model` with allowModelOverride unset is the case this exists for. Standing
// says WHO may ask; the ceiling says WHETHER anyone may.
func TestClampNumeric_anImpermissibleCapabilityIsRefusedWhateverTheValue(t *testing.T) {
	_, _, err := capability.ClampNumeric(1, capability.Bound{Permitted: false, Max: 999_999})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not permitted")
}

// A bound with no Max is not "unlimited" by accident. A capability that forgot
// to declare its ceiling must refuse, not wave everything through — the failure
// direction has to be friction an operator notices, never silent permission.
func TestClampNumeric_anUndeclaredCeilingRefusesRatherThanAllowingAnything(t *testing.T) {
	_, _, err := capability.ClampNumeric(1_000_000, capability.Bound{Permitted: true})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ceiling")
}

// The enumerated form: a model name outside the catalog is refused, never
// substituted. Picking a "closest" model would silently run the session on
// something nobody chose.
func TestClampChoice_refusesAValueOutsideTheAllowedSet(t *testing.T) {
	b := capability.Bound{Permitted: true, Allowed: []string{"small", "large"}}

	got, err := capability.ClampChoice("large", b)
	require.NoError(t, err)
	assert.Equal(t, "large", got)

	_, err = capability.ClampChoice("enormous", b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enormous")
}

func TestClampChoice_anImpermissibleCapabilityIsRefused(t *testing.T) {
	_, err := capability.ClampChoice("small", capability.Bound{Allowed: []string{"small"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not permitted")
}

// An empty Allowed set is not "anything goes" — same fail-closed reasoning as
// an undeclared numeric ceiling.
func TestClampChoice_anEmptyAllowedSetRefuses(t *testing.T) {
	_, err := capability.ClampChoice("small", capability.Bound{Permitted: true})

	require.Error(t, err)
}
