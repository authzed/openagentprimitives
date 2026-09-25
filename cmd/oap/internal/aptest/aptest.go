// Package aptest holds the fixtures and measurement helpers the `oap` CLI's
// tests share across command-family packages: a Globals wired to a fake
// cluster, a runner that captures everything a command wrote, and the
// terminal-rendering probes the list/table assertions are built on.
//
// It is ordinary (non-_test) code because a _test.go helper is reachable only
// from its own package, and these are needed from several. Nothing outside a
// test imports it, so it is never linked into the oap binary.
package aptest

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// GlobalsFor wires a Globals whose Bundle() is the given fake, so a whole RunE
// path runs with no cluster.
func GlobalsFor(b *kube.Bundle) *apcmd.Globals {
	return &apcmd.Globals{
		Namespace: b.Namespace,
		BundleFn:  func() (*kube.Bundle, error) { return b, nil },
	}
}

// Run executes cmd with args and returns everything it wrote to stdout and
// stderr combined. A bytes.Buffer is never a terminal, so this exercises
// exactly the path a piped stdout takes: apcmd.DetectCaps sees a non-file
// writer, the theme comes back colorless, and the output must be byte-clean.
func Run(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	require.NoError(t, cmd.Execute(), "oap %s", strings.Join(args, " "))
	return out.String()
}

// HasANSI reports whether s carries any terminal escape sequence.
func HasANSI(s string) bool { return strings.Contains(s, "\x1b[") }

// PlainTheme and ColorTheme are the two capability corners every rendering
// claim is measured at. They are constructed from explicit Caps rather than
// from a stream because a test binary's stdout is never a terminal:
// tui.Detect would answer "no color" whatever the flag said, so a color-on
// assertion is not otherwise reachable from a test.
func PlainTheme() *tui.Theme { return tui.NewTheme(tui.Caps{Width: 80}) }

// ColorTheme is the color-on corner of the pair described on PlainTheme.
func ColorTheme() *tui.Theme {
	return tui.NewTheme(tui.Caps{TTY: true, Color: true, Width: 80})
}

var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// ANSIPrefixOf returns the first SGR sequence in s, or "" when there is none.
// Two differently-colored strings differ here; two identically-colored ones do
// not, which is what makes it usable as a "these are not the same color" claim.
func ANSIPrefixOf(s string) string { return sgrRE.FindString(s) }

// LineContaining returns the single output line holding substr. Alignment is a
// claim about one row, so the row has to be located before it can be measured;
// requiring exactly one match keeps a fixture that accidentally appears twice
// from making the measurement ambiguous.
func LineContaining(t *testing.T, out, substr string) string {
	t.Helper()
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, substr) {
			found = append(found, l)
		}
	}
	require.Len(t, found, 1, "%q must appear on exactly one line of:\n%s", substr, out)
	return found[0]
}

// DisplayColumnOf reports the terminal column cell starts at in line.
//
// It measures with lipgloss.Width — the same unit the table aligns in, and the
// only unit that agrees with what a user sees. A helper counting bytes or runes
// would move in lockstep with an implementation making the same mistake, so an
// alignment assertion built on it could not fail.
func DisplayColumnOf(t *testing.T, line, cell string) int {
	t.Helper()
	i := strings.Index(line, cell)
	require.NotEqual(t, -1, i, "%q must appear in %q", cell, line)
	return lipgloss.Width(line[:i])
}
