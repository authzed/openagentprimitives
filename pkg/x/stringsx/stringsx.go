// Package stringsx holds small, dependency-free string helpers shared across
// packages that would otherwise duplicate a one-line utility rather than
// pull in an unrelated domain package just to reuse it.
package stringsx

// CapRunes hard-caps s at maxRunes Unicode code points, replacing the final
// rune with "…" when s exceeds the bound. Newlines are preserved — this
// truncates, it does not summarize to a first line. s is returned unchanged
// when maxRunes <= 0 or s already fits.
func CapRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	const suffix = "…"
	return string(rs[:maxRunes-len([]rune(suffix))]) + suffix
}
