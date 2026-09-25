package provider_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// TestByBuiltinResolvesTheDeclaredDirection is the point of ByBuiltin: the
// link between a flow and its catalog entry is DECLARED by the provider
// (builtin:), and a caller holding only a flow name has to follow it that way
// round. Looking the flow up ByID instead is correct for every provider we
// ship today purely because each one's id equals its builtin's name, and wrong
// the first time that stops being true.
func TestByBuiltinResolvesTheDeclaredDirection(t *testing.T) {
	for _, p := range provider.All() {
		if p.Builtin == "" {
			continue
		}
		t.Run(p.ID, func(t *testing.T) {
			got, ok := provider.ByBuiltin(p.Builtin)
			require.True(t, ok, "provider %q declares builtin %q but nothing resolves it", p.ID, p.Builtin)
			assert.Equal(t, p.ID, got.ID)
		})
	}
}

// TestByBuiltinRefusesWhatItCannotAnswer covers the two ways a lookup has no
// single right answer. Ambiguity in particular must not resolve to a first
// match: a caller handed another provider's token shape and verification probe
// would gate a pasted credential against a format it was never meant to have.
func TestByBuiltinRefusesWhatItCannotAnswer(t *testing.T) {
	cases := []struct {
		name string
		flow string
	}{
		{name: "empty flow name: not found, rather than the first provider with no builtin", flow: ""},
		{name: "flow no provider declares: not found", flow: "no-such-flow-for-any-provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := provider.ByBuiltin(tc.flow)
			assert.False(t, ok)
			assert.Nil(t, got)
		})
	}
}

// TestEveryShippedBuiltinNamesAtMostOneProvider is what makes the ambiguity
// arm above a rule rather than dead code: today no two catalog entries declare
// the same builtin, so ByBuiltin always has a single answer. The day one does,
// this fails and the caller relying on a unique answer is told before its
// users are.
func TestEveryShippedBuiltinNamesAtMostOneProvider(t *testing.T) {
	seen := map[string]string{}
	for _, p := range provider.All() {
		if p.Builtin == "" {
			continue
		}
		if prev, dup := seen[p.Builtin]; dup {
			t.Errorf("builtin %q is declared by both %q and %q; ByBuiltin can no longer answer for it",
				p.Builtin, prev, p.ID)
		}
		seen[p.Builtin] = p.ID
	}
	assert.NotEmpty(t, seen, "no provider declares a builtin; this test would pass vacuously")
}
