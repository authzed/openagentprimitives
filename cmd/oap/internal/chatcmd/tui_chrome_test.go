package chatcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// This file covers what `oap agent chat` — one of oap's two long-lived bubbletea
// models, the other being `oap tools mcp probe` — takes from pkg/cli/tui. The
// probe model is held to the same two rules in its own package's
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
// can any test prove --no-color reaches the theme: a test binary's stdout is
// never a terminal, so tui.Detect answers "no color" whatever the flag says.

// TestChatTUIRenderersFollowTheThemesColorCapability renders each surface of
// the chat TUI twice, changing only the theme's Caps.Color, and requires the
// output to carry escape codes in exactly one of the two runs.
//
// Both directions matter and neither is sufficient alone. Color-off alone would
// stay green if a renderer stopped styling anything at all; color-on alone would
// stay green if a renderer hardcoded its colors and ignored Caps entirely. Read
// together they say the styling is the theme's and the theme is consulted.
//
// chatModel.View() is deliberately absent: it embeds bubbles' textinput, whose
// blinking cursor styles itself off lipgloss's global renderer rather than this
// theme, so its escape codes would not be evidence about anything in this repo.
// Every part of that view this repo composes is covered as its own case.
func TestChatTUIRenderersFollowTheThemesColorCapability(t *testing.T) {
	interaction := channelevents.InteractionRequestPayload{
		RequestRef: "r1",
		Category:   "tool_approval",
		Lead:       "The agent wants to run a tool.",
		Actions: []channelevents.InteractionAction{
			{Kind: channelevents.ActionKindDecision, ID: "approve", Label: "Approve"},
			{Kind: channelevents.ActionKindDecision, ID: "deny", Label: "Deny"},
		},
	}
	plan := channelevents.PlanUpdatePayload{
		PlanName: "ship it",
		Items:    []channelevents.PlanItemRef{{Label: "write the code", Status: "done"}},
	}
	timeline := []timelineItem{
		{kind: itemUser, text: "do the thing"},
		{kind: itemAgent, text: "done"},
	}

	cases := []struct {
		name   string
		render func(th *tui.Theme) string
	}{
		{
			name:   "timeline",
			render: func(th *tui.Theme) string { return renderTimeline(timeline, 60, th) },
		},
		{
			name:   "status line",
			render: func(th *tui.Theme) string { return renderStatusLine("", connConnected, &plan, false, 60, th) },
		},
		{
			name:   "plan overlay",
			render: func(th *tui.Theme) string { return renderPlanOverlay(plan, 60, 20, th) },
		},
		{
			name: "interaction modal",
			render: func(th *tui.Theme) string {
				return renderInteractionModal(interaction, local.SessionRef{Namespace: "default", Name: "demo-session"}, "demo-session", 60, 20, th)
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

// TestChatTUIConstructorResolvesAColorlessThemeOffANonTerminal pins that the
// model's theme comes from measured capabilities rather than from a hardcoded
// profile. It is what fails if the constructor is changed to build a
// color-enabled theme unconditionally.
//
// It does NOT prove the --no-color flag is threaded: this process's stdout is
// not a terminal, so tui.Detect would report no color for either argument. What
// it proves is that the constructor asked, and stored the answer.
func TestChatTUIConstructorResolvesAColorlessThemeOffANonTerminal(t *testing.T) {
	chat := newChatModel("demo-agent", "s1", true)
	require.NotNil(t, chat.th, "the chat model must carry a theme")
	assert.False(t, chat.th.Caps.Color, "a non-terminal stdout must yield a colorless chat theme")

	// The renderers this repo owns must then be byte-clean, so that a theme
	// reporting Color=false is not merely recorded but obeyed.
	assert.False(t, aptest.HasANSI(renderTimeline([]timelineItem{{kind: itemAgent, text: "hi"}}, 60, chat.th)),
		"a colorless chat theme must render the timeline byte-clean")
}

// TestChatTUIKeepsEveryLineWithinTheTerminalWidth is the one piece of
// tui.Chrome this model does adopt: Chrome guarantees no rendered line exceeds
// the terminal width, and the model applies that rule at the width
// WindowSizeMsg gave it rather than the one Theme.Caps froze at construction.
//
// The title is the overflow risk (an agent class name), and an over-wide title
// is not merely ugly: lipgloss.JoinVertical pads every block to the widest one,
// so a 200-column title drags the whole frame out to 200 columns and the panes
// wrap.
func TestChatTUIKeepsEveryLineWithinTheTerminalWidth(t *testing.T) {
	overlong := "over-long-name-" + strings.Repeat("x", 200)
	require.Greater(t, len(overlong), 100, "precondition: the fixture must not already fit the terminal")

	m := sized(newChatModel(overlong, "s1", true))
	require.Equal(t, 80, m.width, "precondition: sized() gives an 80-column terminal")
	assert.LessOrEqual(t, aptest.MaxLineWidth(t, m.View()), 80, "no line may exceed the terminal width")
}
