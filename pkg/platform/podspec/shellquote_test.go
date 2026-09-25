package podspec

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// %q is GO-literal quoting, not shell quoting. It escapes " and \ and leaves $
// and backtick intact, and the mount-unpack script spliced its output into a
// double-quoted `sh -c` word where both still expand. The paths it quoted
// derive from SpiceboxMount.Name, which carries no Pattern and no MaxLength and
// is deliberately not constrained to a DNS-1123 label.
func TestShellQuote_NeutralisesWhatGoLiteralQuotingLeavesLive(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "command substitution via $()", in: `a$(id)b`},
		{name: "command substitution via backticks", in: "a`id`b"},
		{name: "variable expansion", in: `a${HOME}b`},
		{name: "a statement separator", in: `a; rm -rf /; b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := shellQuote(tc.in)

			assert.Equal(t, "'"+tc.in+"'", q,
				"single quotes make every metacharacter literal to the shell")
			assert.NotContains(t, fmt.Sprintf("%q", tc.in), `\$`,
				"precondition: %q does NOT escape these, which is why it was the wrong tool")
		})
	}
}

// The one character single quoting cannot pass through unaided is the single
// quote itself; the '\'' idiom closes, escapes, and reopens.
func TestShellQuote_EscapesAnEmbeddedSingleQuote(t *testing.T) {
	assert.Equal(t, `'a'\''; id; b'`, shellQuote(`a'; id; b`))
}
