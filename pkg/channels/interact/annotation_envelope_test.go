package interact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAnnotationEnvelope_NumbersAndDelimitsUntrustedDOM(t *testing.T) {
	out, err := buildAnnotationEnvelope([]annotation{
		{Index: 1, Target: "element", Comment: "reword the hero", Intent: "change", Severity: "high",
			TagName: "button", ElementPath: "#cta", ElementText: "Join now"},
		{Index: 2, Target: "region", Comment: "why empty?", TagName: "section", ElementPath: "#hero"},
	})
	require.NoError(t, err)

	// Numbered + addressable.
	assert.Contains(t, out, "Annotation 1")
	assert.Contains(t, out, "Annotation 2")
	// User fields are trusted (outside the wrapper).
	assert.Contains(t, out, "reword the hero")
	assert.Contains(t, out, "change")
	// DOM fields are inside a nonce-delimited untrusted envelope.
	assert.Contains(t, out, `<untrusted-annotations nonce="`)
	// The opening and closing nonce match, and the DOM JSON sits between them.
	open := between(t, out, `<untrusted-annotations nonce="`, `"`)
	require.NotEmpty(t, open)
	assert.Contains(t, out, `</untrusted-annotations nonce="`+open+`">`)
	assert.Contains(t, out, "#cta") // DOM path is present (inside the wrapper)
}

func TestBuildAnnotationEnvelope_NonceIsFreshPerBatch(t *testing.T) {
	a, _ := buildAnnotationEnvelope([]annotation{{Index: 1, Comment: "x"}})
	b, _ := buildAnnotationEnvelope([]annotation{{Index: 1, Comment: "x"}})
	assert.NotEqual(t,
		between(t, a, `nonce="`, `"`), between(t, b, `nonce="`, `"`),
		"each batch must get an unpredictable fresh nonce")
}

// between returns the substring of s between the first occurrence of a and the
// next occurrence of b after it.
func between(t *testing.T, s, a, b string) string {
	t.Helper()
	i := strings.Index(s, a)
	require.GreaterOrEqual(t, i, 0, "prefix %q not found", a)
	i += len(a)
	j := strings.Index(s[i:], b)
	require.GreaterOrEqual(t, j, 0, "suffix %q not found", b)
	return s[i : i+j]
}
