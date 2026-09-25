package trifecta_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
)

// TestOnlyAllThreeRefuses walks every combination.
//
// "Any two is fine" is the claim most likely to be quietly weakened later into
// "any two is fine unless…", so each two-leg pair gets its own case rather than
// being covered by one representative. The design permits each of them
// deliberately: untrusted input a child cannot act on, sensitive data a child
// cannot exfiltrate, a capable child fed nothing hostile.
func TestOnlyAllThreeRefuses(t *testing.T) {
	cases := []struct {
		name string
		legs trifecta.Legs
		want bool // refused
	}{
		{name: "nothing", legs: trifecta.Legs{}, want: false},
		{name: "untrusted alone", legs: trifecta.Legs{Untrusted: true}, want: false},
		{name: "sensitive alone", legs: trifecta.Legs{Sensitive: true}, want: false},
		{name: "consequential alone", legs: trifecta.Legs{Consequential: true}, want: false},
		{
			name: "untrusted + sensitive: hostile content and private data, but the child cannot act",
			legs: trifecta.Legs{Untrusted: true, Sensitive: true},
		},
		{
			name: "untrusted + consequential: hostile content and a capable child, but nothing private to take",
			legs: trifecta.Legs{Untrusted: true, Consequential: true},
		},
		{
			name: "sensitive + consequential: private data and a capable child, but nothing hostile driving it",
			legs: trifecta.Legs{Sensitive: true, Consequential: true},
		},
		{
			name: "all three",
			legs: trifecta.Legs{Untrusted: true, Sensitive: true, Consequential: true},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := trifecta.Evaluate(tc.legs)
			assert.Equal(t, tc.want, v.Refused)
		})
	}
}

// TestARefusalNamesTheLegs keeps a refusal actionable.
//
// An operator seeing "refused" learns nothing about what to change. Naming the
// legs says whether to narrow the child's TOOLS, narrow its DATA, or fix where
// the content came from — three different remedies, and the reason is the only
// thing that distinguishes them.
func TestARefusalNamesTheLegs(t *testing.T) {
	v := trifecta.Evaluate(trifecta.Legs{Untrusted: true, Sensitive: true, Consequential: true})

	assert.True(t, v.Refused)
	for _, want := range []string{"untrusted", "sensitive", "consequential"} {
		assert.True(t, strings.Contains(strings.ToLower(v.Reason), want),
			"the refusal must name the %q leg so an operator knows which of three remedies applies; got %q", want, v.Reason)
	}
}

// TestAnAllowedVerdictStillReportsTheLegs.
//
// Two legs is the interesting near-miss, and logging mode exists to collect
// exactly those. A verdict that only spoke when refusing would make the
// dataset the mode is for impossible to build.
func TestAnAllowedVerdictStillReportsTheLegs(t *testing.T) {
	v := trifecta.Evaluate(trifecta.Legs{Untrusted: true, Consequential: true})

	assert.False(t, v.Refused)
	assert.Equal(t, 2, v.LegCount, "a near-miss must be countable without re-deriving it from the reason text")
}

// TestTheZeroVerdictIsNotARefusal pins the zero value.
//
// Unlike Grade, where the conservative default is to route, a zero Verdict
// means "nothing was judged" — and it must not read as a refusal, or a caller
// that forgot to evaluate would block every delegation and look like a working
// gate. The fail-safe direction here is upstream: Derive errors rather than
// returning empty legs.
func TestTheZeroVerdictIsNotARefusal(t *testing.T) {
	var v trifecta.Verdict
	assert.False(t, v.Refused)
	assert.Zero(t, v.LegCount)
}
