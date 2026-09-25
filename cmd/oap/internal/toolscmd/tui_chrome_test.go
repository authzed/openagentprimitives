package toolscmd

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// This file covers what `oap tools mcp probe` — one of oap's two long-lived
// bubbletea models, the other being `oap agent chat` — takes from pkg/cli/tui.
// The chat model is held to the same two rules in chatcmd's own
// tui_chrome_test.go; the pair is deliberate, so a change to either should be
// checked against both.
//
// What these tests can prove: that each renderer's output changes with the
// theme's Caps.Color, that the constructor resolves a colorless theme, and that
// no line the model composes exceeds the width it was given.
//
// What they cannot prove: what a terminal DRAWS. A bubbletea View() is a string
// this process assembles; whether it lands on screen as intended is not
// observable without a pseudo-terminal, and nothing here claims otherwise. Nor
// can any test in this package prove --no-color reaches the theme: a test
// binary's stdout is never a terminal, so tui.Detect answers "no color" whatever
// the flag says (the same limit TestListCommandsAcceptNoColorAndStayPlain
// records for the list commands).

// probeFixture is the tool list the probe cases render.
func probeFixture() []probe.Tool {
	return []probe.Tool{
		{Name: "fetch_data", Description: "Fetches.", Annotations: probe.Annotations{ReadOnlyHint: true}},
		{Name: "write_record", Description: "Writes.", Annotations: probe.Annotations{DestructiveHint: true}},
	}
}

// sizedProbe returns a probe model that has seen a WindowSizeMsg, so layout and
// viewport are populated and View() composes the full frame.
func sizedProbe(t *testing.T, m probeTUIModel, width, height int) probeTUIModel {
	t.Helper()
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	pm, ok := next.(probeTUIModel)
	require.True(t, ok, "Update must return a probeTUIModel")
	return pm
}

// TestProbeTUIRenderersFollowTheThemesColorCapability renders each surface of
// the probe TUI twice, changing only the theme's Caps.Color, and requires the
// output to carry escape codes in exactly one of the two runs.
//
// Both directions matter and neither is sufficient alone. Color-off alone would
// stay green if a renderer stopped styling anything at all; color-on alone would
// stay green if a renderer hardcoded its colors and ignored Caps entirely. Read
// together they say the styling is the theme's and the theme is consulted.
func TestProbeTUIRenderersFollowTheThemesColorCapability(t *testing.T) {
	cases := []struct {
		name   string
		render func(th *tui.Theme) string
	}{
		{
			name: "list pane (cursor row highlighted)",
			render: func(th *tui.Theme) string {
				return probeListView(probeFixture(), 0, 0, 2, 40, th)
			},
		},
		{
			name: "detail pane",
			render: func(th *tui.Theme) string {
				return probeDetailContent(probeFixture()[0], true, 40, th)
			},
		},
		{
			name: "full view",
			render: func(th *tui.Theme) string {
				m := newProbeTUIModelWithTheme("demo server", probeFixture(), th)
				return sizedProbe(t, m, 100, 30).View()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+": plain with color off, styled with color on", func(t *testing.T) {
			plain := tc.render(aptest.PlainTheme())
			assert.False(t, aptest.HasANSI(plain), "color off must emit no escape codes:\n%q", plain)

			colored := tc.render(aptest.ColorTheme())
			assert.True(t, aptest.HasANSI(colored), "color on must emit escape codes:\n%q", colored)
		})
	}
}

// TestProbeTUIConstructorResolvesAColorlessThemeOffANonTerminal pins that the
// model's theme comes from measured capabilities rather than from a hardcoded
// profile. It is what fails if the constructor is changed to build a
// color-enabled theme unconditionally.
//
// It does NOT prove the --no-color flag is threaded: this process's stdout is
// not a terminal, so tui.Detect would report no color for either argument. What
// it proves is that the constructor asked, and stored the answer.
func TestProbeTUIConstructorResolvesAColorlessThemeOffANonTerminal(t *testing.T) {
	pr := newProbeTUIModel("demo server", probeFixture(), true)
	require.NotNil(t, pr.th, "the probe model must carry a theme")
	assert.False(t, pr.th.Caps.Color, "a non-terminal stdout must yield a colorless probe theme")

	// The renderers this repo owns must then be byte-clean, so that a theme
	// reporting Color=false is not merely recorded but obeyed.
	assert.False(t, aptest.HasANSI(sizedProbe(t, pr, 100, 30).View()),
		"a colorless probe theme must render the probe view byte-clean")
}

// TestProbeTUIKeepsEveryLineWithinTheTerminalWidth is the one piece of
// tui.Chrome this model does adopt: Chrome guarantees no rendered line exceeds
// the terminal width, and the model applies that rule at the width
// WindowSizeMsg gave it rather than the one Theme.Caps froze at construction.
//
// The title is the overflow risk (an MCP server's own name), and an over-wide
// title is not merely ugly: lipgloss.JoinVertical pads every block to the
// widest one, so a 200-column title drags the whole frame out to 200 columns
// and the panes wrap.
func TestProbeTUIKeepsEveryLineWithinTheTerminalWidth(t *testing.T) {
	overlong := "over-long-name-" + strings.Repeat("x", 200)
	require.Greater(t, len(overlong), 100, "precondition: the fixture must not already fit the terminal")

	m := sizedProbe(t, newProbeTUIModel(overlong, probeFixture(), true), 100, 30)
	require.Equal(t, 100, m.width, "precondition: the model took the window width")
	assert.LessOrEqual(t, aptest.MaxLineWidth(t, m.View()), 100, "no line may exceed the terminal width")
}
