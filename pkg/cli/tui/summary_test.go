package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSummaryAlignsLabelsAndMarksEachLine(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, RenderSummary(&buf, NewTheme(Caps{}), []Note{
		{Label: "Agent", Value: "demo-agent"},
		{Label: "Channel resource", Value: "demo-channel"},
	}))

	got := buf.String()
	assert.Contains(t, got, "✓ Agent")
	assert.Contains(t, got, "demo-agent")
	assert.Contains(t, got, "✓ Channel resource")
	assert.Contains(t, got, "demo-channel")
	assert.False(t, hasANSI(got), "uncolored theme must render a byte-clean summary")

	lines := splitNonEmpty(got)
	require.Len(t, lines, 2, "one line per note")
	assert.Equal(t, displayColumnOf(t, lines[0], "demo-agent"), displayColumnOf(t, lines[1], "demo-channel"),
		"values must align in a column regardless of label width")
}

func TestRenderSummaryWithNoNotesWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, RenderSummary(&buf, NewTheme(Caps{}), nil))
	assert.Empty(t, buf.String(), "an empty summary must not emit stray blank lines")
}

func TestRenderSummaryLabelsOfEveryWidthAlignTheValueColumn(t *testing.T) {
	// One case per unit a padding implementation could plausibly measure in.
	// Each pairs an ASCII label (bytes == runes == columns) with a label the
	// three units disagree about, so a summary padded in the wrong unit
	// misaligns the value column and fails here:
	//
	//	"Café"  4 runes, 5 bytes, 4 columns  → byte counting is wrong
	//	"日本"   2 runes, 6 bytes, 4 columns  → rune counting is wrong
	//	"🎯 Hit" 5 runes, 8 bytes, 6 columns  → rune counting is wrong
	cases := []struct {
		name  string
		notes []Note
	}{
		{
			name: "multi-byte label: value column matches the ASCII row",
			notes: []Note{
				{Label: "Status", Value: "active"},
				{Label: "Café", Value: "open"},
			},
		},
		{
			name: "wide CJK label: value column matches the ASCII row",
			notes: []Note{
				{Label: "Status", Value: "active"},
				{Label: "日本", Value: "open"},
			},
		},
		{
			name: "emoji label: value column matches the ASCII row",
			notes: []Note{
				{Label: "Status", Value: "active"},
				{Label: "🎯 Hit", Value: "open"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, RenderSummary(&buf, NewTheme(Caps{}), tc.notes))

			lines := splitNonEmpty(buf.String())
			require.Len(t, lines, 2, "one line per note")
			assert.Equal(t,
				displayColumnOf(t, lines[0], tc.notes[0].Value),
				displayColumnOf(t, lines[1], tc.notes[1].Value),
				"both values must start at the same display column")
		})
	}
}

func TestRenderSummaryErrorsOnWriteFailure(t *testing.T) {
	fw := &failingWriter{}
	err := RenderSummary(fw, NewTheme(Caps{}), []Note{
		{Label: "Test", Value: "value"},
	})

	require.Error(t, err, "RenderSummary must return error when writer fails")
	assert.True(t, errors.Is(err, errWriterFailed), "error must wrap the writer failure")
	assert.ErrorContains(t, err, "write summary", "error message must mention the operation")
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// displayColumnOf reports the terminal column cell starts at in line.
//
// It measures with lipgloss.Width — the same unit the renderers align in, and
// the only unit that agrees with what the user sees. A helper that counted
// bytes or runes instead would move in lockstep with an implementation making
// the same mistake, so an alignment assertion built on it could not fail.
func displayColumnOf(t *testing.T, line, cell string) int {
	t.Helper()
	i := strings.Index(line, cell)
	require.NotEqual(t, -1, i, "%q must appear in %q", cell, line)
	return lipgloss.Width(line[:i])
}

var errWriterFailed = errors.New("mock writer failed")

type failingWriter struct{}

func (f *failingWriter) Write(p []byte) (int, error) {
	return 0, errWriterFailed
}
