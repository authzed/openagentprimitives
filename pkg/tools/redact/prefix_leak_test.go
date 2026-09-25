package redact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRedactInString_LongerSecretSharingAPrefixIsFullyRedacted guards the
// order-dependence in RedactInString: it walks a map, so when one registered
// secret is a prefix of another, iteration order decides whether the shorter
// one is substituted first and leaves the longer one's tail in cleartext.
//
// Redaction is the last thing standing between a credential and a transcript,
// so it must not depend on map ordering. The loop runs enough times that a
// randomized order cannot pass by luck.
func TestRedactInString_LongerSecretSharingAPrefixIsFullyRedacted(t *testing.T) {
	const (
		short = "tok_abc"
		long  = "tok_abc_LONGTAIL"
		tail  = "_LONGTAIL"
	)

	for i := 0; i < 200; i++ {
		r := New()
		r.RegisterSensitive(short, Descriptor{Description: "short token", Kind: "env", Name: "SHORT"})
		r.RegisterSensitive(long, Descriptor{Description: "long token", Kind: "env", Name: "LONG"})

		got := r.RedactInString("value=" + long + " end")

		assert.NotContains(t, got, tail,
			"the longer secret's tail survived redaction (iteration %d): %q", i, got)
		assert.NotContains(t, got, long,
			"the longer secret survived verbatim (iteration %d): %q", i, got)
	}
}

// TestRedactInString_ReplacesLongestFirst pins the ordering rule directly, so a
// future rewrite that reintroduces map-order iteration fails here with a clear
// cause rather than only as a flake in the test above.
func TestRedactInString_ReplacesLongestFirst(t *testing.T) {
	r := New()
	r.RegisterSensitive("abc", Descriptor{Description: "short token", Kind: "env", Name: "SHORT"})
	r.RegisterSensitive("abcdef", Descriptor{Description: "long token", Kind: "env", Name: "LONG"})

	got := r.RedactInString("abcdef")

	assert.False(t, strings.Contains(got, "def"),
		"the longer secret must be matched before its own prefix, got %q", got)
}
