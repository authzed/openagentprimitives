// Package icons hosts identityd's favicon discovery + cache + serve
// pipeline.
package icons

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
)

// FallbackSVG returns SVG bytes for a credential when favicon discovery finds
// nothing: a 64×64 rounded square tinted from sha256(credName), with the
// credential's first character centered in white. Identical bytes for identical
// credName, so a user builds recognition across the portal, Slack Home and DM
// surfaces. Empty credName renders "?".
func FallbackSVG(credName string) []byte {
	sum := sha256.Sum256([]byte(credName))
	// First 2 bytes of the hash → hue (0-359).
	hue := int(binary.BigEndian.Uint16(sum[:2])) % 360

	letter := "?"
	for _, r := range credName {
		// Use the first non-whitespace rune.
		if !unicode.IsSpace(r) {
			letter = strings.ToUpper(string(r))
			break
		}
	}

	// Single-line, inline-styled SVG: nothing to fetch, minimal bytes.
	svg := fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">`+
			`<rect width="64" height="64" rx="12" fill="hsl(%d, 65%%, 50%%)"/>`+
			`<text x="32" y="44" font-size="36" text-anchor="middle" font-family="-apple-system, Segoe UI, sans-serif" font-weight="600" fill="white">%s</text>`+
			`</svg>`,
		hue, escapeXMLText(letter),
	)
	return []byte(svg)
}

// escapeXMLText escapes the five XML-significant characters, allocating only
// when one is present.
func escapeXMLText(s string) string {
	if !strings.ContainsAny(s, `<>&"'`) {
		return s
	}
	return strings.NewReplacer("<", "&lt;", ">", "&gt;", "&", "&amp;", `"`, "&quot;", "'", "&#39;").Replace(s)
}
