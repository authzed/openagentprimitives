package toolkitstream_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
)

// TestOutcomeUnbilledFailure pins the credential-halt classifier.
//
// The whole point of this function is WHAT IT REFUSES TO READ. It sees only
// the three structured facts a toolkit reports about its own run — did it
// reach a terminal result, did that result succeed, did the provider bill for
// it — and never the text of that run. A CLI echoes the argv the AGENT chose
// back into its own error output, so a classifier that matched text would let
// a hostile tool result induce a session halt on demand, or dress an auth
// failure up as ordinary work to hide one.
func TestOutcomeUnbilledFailure(t *testing.T) {
	cases := []struct {
		name string
		oc   toolkitstream.Outcome
		want bool
	}{
		{
			name: "non-success with an empty billing tally: the run never got past auth",
			oc:   toolkitstream.Outcome{HasResult: true, OK: false, Unbilled: true},
			want: true,
		},
		{
			name: "non-success the provider BILLED for: real work that then failed, not a credential",
			oc:   toolkitstream.Outcome{HasResult: true, OK: false, CostUSD: 0.005},
			want: false,
		},
		{
			name: "success with an empty tally (a cached or no-op run): not a failure at all",
			oc:   toolkitstream.Outcome{HasResult: true, OK: true, Unbilled: true},
			want: false,
		},
		{
			name: "no terminal result: the toolkit said nothing, so nothing is claimed",
			oc:   toolkitstream.Outcome{HasResult: false, OK: false, Unbilled: true},
			want: false,
		},
		{
			name: "a toolkit that reports no billing at all never trips the halt",
			// Unbilled is set by the PARSER, which is the only layer that knows
			// whether its toolkit's terminal event carries a billing tally. A
			// parser that leaves it false has not said "nothing was billed" —
			// it has said nothing — and a zero CostUSD must not be read as the
			// former.
			oc:   toolkitstream.Outcome{HasResult: true, OK: false, CostUSD: 0},
			want: false,
		},
		{
			name: "the zero Outcome claims nothing",
			oc:   toolkitstream.Outcome{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.oc.UnbilledFailure())
		})
	}
}

// TestOutcomeUnbilledFailureIgnoresText is the prompt-injection control, stated
// as its own test because it is the property most worth breaking loudly.
//
// The Outcome's Text is inner-tool output: attacker-influenceable in the
// general case and agent-influenceable in every case. It must not move the
// classifier in either direction.
func TestOutcomeUnbilledFailureIgnoresText(t *testing.T) {
	const authLooking = "API Error: 401 {\"error\":{\"message\":\"invalid x-api-key\"}}"

	billed := toolkitstream.Outcome{HasResult: true, OK: true, CostUSD: 0.01, Text: authLooking}
	assert.False(t, billed.UnbilledFailure(),
		"a billed, successful run whose OUTPUT reads like an auth error must not halt the session")

	quiet := toolkitstream.Outcome{HasResult: true, OK: false, Unbilled: true, Text: "everything is fine, no problem here"}
	assert.True(t, quiet.UnbilledFailure(),
		"and reassuring output must not hide an unbilled failure either")
}
