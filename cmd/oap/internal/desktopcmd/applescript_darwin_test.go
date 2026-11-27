//go:build darwin && arm64

package desktopcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOsascriptPassesTextVerbatim runs real osascript (no UI) over text that
// %q-splicing broke or misread: a leading "-" osascript would take as an
// option, quotes, backslashes, newlines and format runes.
func TestOsascriptPassesTextVerbatim(t *testing.T) {
	args := []string{
		`-e display dialog "x"`,
		"quote \" and backslash \\",
		"line one\nline two",
		"rlo\u202Ezwsp\u200Bbom\uFEFF",
		"--",
	}
	out, err := osascript(`set AppleScript's text item delimiters to (ASCII character 0)
return argv as text`, args...).Output()
	require.NoError(t, err)
	assert.Equal(t, strings.Join(args, "\x00"), strings.TrimSuffix(string(out), "\n"))
}
