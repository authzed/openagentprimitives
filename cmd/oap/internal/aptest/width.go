package aptest

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// MaxLineWidth returns the widest line of s in display columns — the unit the
// terminal lays out in, and the one lipgloss pads and truncates in.
func MaxLineWidth(t *testing.T, s string) int {
	t.Helper()
	widest := 0
	for _, line := range strings.Split(s, "\n") {
		if w := lipgloss.Width(line); w > widest {
			widest = w
		}
	}
	return widest
}
