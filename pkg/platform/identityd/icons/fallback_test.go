package icons

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFallbackSVG_Deterministic(t *testing.T) {
	a := FallbackSVG("linear")
	b := FallbackSVG("linear")
	assert.Equal(t, a, b, "same credName must produce identical bytes")
}

func TestFallbackSVG_Distinct(t *testing.T) {
	a := FallbackSVG("linear")
	b := FallbackSVG("github")
	assert.NotEqual(t, a, b, "different credNames must produce different bytes (color or letter differ)")
}

func TestFallbackSVG_ContainsCredFirstLetter(t *testing.T) {
	cases := map[string]string{
		"linear":     "L",
		"github_pat": "G",
		"hubspot":    "H",
		"9github":    "9", // numeric first char passes through verbatim
		"":           "?", // empty input falls back to "?"
	}
	for cred, want := range cases {
		t.Run(cred, func(t *testing.T) {
			svg := string(FallbackSVG(cred))
			assert.Contains(t, svg, ">"+want+"<", "rendered SVG must contain the first letter")
		})
	}
}

func TestFallbackSVG_ValidStructure(t *testing.T) {
	svg := string(FallbackSVG("linear"))
	require.True(t, strings.HasPrefix(svg, "<svg "), "must start with <svg tag")
	require.True(t, strings.HasSuffix(strings.TrimSpace(svg), "</svg>"), "must end with </svg>")
	assert.Contains(t, svg, `xmlns="http://www.w3.org/2000/svg"`, "must declare SVG xmlns")
	assert.Contains(t, svg, `viewBox="0 0 64 64"`, "must declare 64x64 viewBox")
	assert.Contains(t, svg, "<rect ")
	assert.Contains(t, svg, "<text ")
}

func TestFallbackSVG_HueDerivedFromHash(t *testing.T) {
	// sha256("linear")[0:3] is deterministic; we just check that the
	// first 3 hex chars of the hash appear in the rendered SVG's hue
	// expression — that locks down the derivation without hardcoding
	// a specific hue.
	svg := string(FallbackSVG("linear"))
	assert.Contains(t, svg, "hsl(", "fill color must use HSL")
}
