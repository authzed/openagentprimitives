package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
)

// entryFor marshals a toolsession.Event into a memory.Entry's Content,
// the same shape StreamEntries delivers to RenderToolSessionEntry.
func entryFor(t *testing.T, ev toolsession.Event) memory.Entry {
	t.Helper()
	raw, err := json.Marshal(ev)
	require.NoError(t, err, "marshal toolsession.Event")
	return memory.Entry{Kind: toolsession.KindName, Content: raw}
}

func TestRenderToolSessionEntry(t *testing.T) {
	cases := []struct {
		name        string
		entry       memory.Entry
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "tool_use_start: prints tool name and summary",
			entry:       entryFor(t, toolsession.Event{EventType: "tool_use_start", ToolName: "Edit", Summary: "README.md"}),
			wantContain: []string{"Edit", "README.md"},
		},
		{
			name:        "tool_use_stop OK=true: prints tool name and success mark",
			entry:       entryFor(t, toolsession.Event{EventType: "tool_use_stop", ToolName: "Edit", Summary: "README.md", OK: true}),
			wantContain: []string{"Edit", "✓"},
		},
		{
			name:        "tool_use_stop OK=false: prints error mark",
			entry:       entryFor(t, toolsession.Event{EventType: "tool_use_stop", ToolName: "Edit", Summary: "README.md", OK: false}),
			wantContain: []string{"Edit", "✗"},
		},
		{
			name:        "result: prints done with duration and cost",
			entry:       entryFor(t, toolsession.Event{EventType: "result", OK: true, DurationMs: 12300, CostUSD: 0.0421}),
			wantContain: []string{"done", "12300", "0.0421"},
		},
		{
			name:        "text_delta: prints the streamed text",
			entry:       entryFor(t, toolsession.Event{EventType: "text_delta", Text: "hello"}),
			wantContain: []string{"hello"},
		},
		{
			name:        "unknown event type: falls back to printing the type",
			entry:       entryFor(t, toolsession.Event{EventType: "mystery"}),
			wantContain: []string{"mystery"},
		},
		{
			name:        "malformed content: prints a decode error",
			entry:       memory.Entry{Kind: toolsession.KindName, Content: []byte("{bad")},
			wantContain: []string{"decode error"},
		},
	}

	// The zero Caps are the colorless theme, so every assertion below reads a
	// plain substring rather than one buried in an escape sequence.
	th := tui.NewTheme(tui.Caps{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			RenderToolSessionEntry(&buf, th, tc.entry)
			got := buf.String()
			for _, want := range tc.wantContain {
				require.Contains(t, got, want)
			}
			for _, absent := range tc.wantAbsent {
				require.NotContains(t, got, absent)
			}
			require.True(t, strings.HasSuffix(got, "\n"), "output must end with a newline")
		})
	}
}

// turnFixtures are the transcript shapes the two renderers below are measured
// on: one per role banner, plus the usage badge and each content-block kind, so
// that every style either renderer reaches for is exercised.
func turnFixtures() []struct {
	name   string
	render func(io.Writer, *tui.Theme)
} {
	turn := func(role string, usage *memory.Usage, blocks ...memory.ContentBlock) memory.Turn {
		return memory.Turn{Role: role, Index: 1, Usage: usage, Content: blocks}
	}
	text := memory.ContentBlock{Type: "text", Text: "hello"}
	toolUse := memory.ContentBlock{Type: "tool_use", ToolUse: &memory.ToolUseBlock{Name: "Edit", Input: []byte(`{}`)}}
	toolErr := memory.ContentBlock{Type: "tool_result", ToolResult: &memory.ToolResultBlock{IsError: true, Content: "boom"}}

	return []struct {
		name   string
		render func(io.Writer, *tui.Theme)
	}{
		{"user turn", func(w io.Writer, th *tui.Theme) { RenderTurn(w, th, turn("user", nil, text)) }},
		{"assistant turn with a usage badge", func(w io.Writer, th *tui.Theme) {
			RenderTurn(w, th, turn("assistant", &memory.Usage{InputTokens: 3, OutputTokens: 4}, text))
		}},
		{"system_note turn", func(w io.Writer, th *tui.Theme) { RenderTurn(w, th, turn("system_note", nil, text)) }},
		{"turn carrying a tool_use block", func(w io.Writer, th *tui.Theme) { RenderTurn(w, th, turn("assistant", nil, toolUse)) }},
		{"turn carrying a failed tool_result", func(w io.Writer, th *tui.Theme) { RenderTurn(w, th, turn("assistant", nil, toolErr)) }},
		{"tool-session start event", func(w io.Writer, th *tui.Theme) {
			RenderToolSessionEntry(w, th, memory.Entry{Kind: toolsession.KindName,
				Content: mustMarshal(toolsession.Event{EventType: "tool_use_start", ToolName: "Edit", Summary: "README.md"})})
		}},
		{"tool-session stop event", func(w io.Writer, th *tui.Theme) {
			RenderToolSessionEntry(w, th, memory.Entry{Kind: toolsession.KindName,
				Content: mustMarshal(toolsession.Event{EventType: "tool_use_stop", ToolName: "Edit", OK: true})})
		}},
	}
}

func mustMarshal(ev toolsession.Event) []byte {
	raw, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestTranscriptRenderersEmitNoANSIUnderAColorlessTheme extends the guard the
// list commands carry (hasANSI, in list_output_test.go) to the two streaming
// transcript renderers. `oap agent run`, `oap session logs` and `oap agent logs`
// are piped into grep and jq constantly, and an escape code in a log line
// corrupts whatever is reading it — so a --no-color or non-terminal run must be
// byte-clean.
//
// Each fixture is asserted BOTH ways. The colorless assertion alone would pass
// on a renderer that had stopped styling altogether, and would also pass on one
// that never routed through the theme at all as long as it happened to be
// plain; requiring the colored theme to produce escape codes for the same input
// is what makes the pair a real claim about the theme being consulted.
func TestTranscriptRenderersEmitNoANSIUnderAColorlessTheme(t *testing.T) {
	colorless := tui.NewTheme(tui.Caps{})
	colored := tui.NewTheme(tui.Caps{TTY: true, Color: true, Width: 80})

	for _, tc := range turnFixtures() {
		t.Run(tc.name+": plain under a colorless theme, colored under a color-enabled one", func(t *testing.T) {
			var plain bytes.Buffer
			tc.render(&plain, colorless)
			assert.False(t, aptest.HasANSI(plain.String()),
				"a colorless theme must leave no escape codes for a pipe to carry:\n%q", plain.String())

			var rich bytes.Buffer
			tc.render(&rich, colored)
			assert.True(t, aptest.HasANSI(rich.String()),
				"a color-enabled theme must reach these lines, or the colorless assertion above proves nothing:\n%q", rich.String())
		})
	}
}

// TestRenderTurnNamesTheSpeaker pins the banner each role gets. The renderer's
// only job on the first line is to say who is talking; a role it does not know
// falls through to its own name rather than being dropped or mislabelled.
func TestRenderTurnNamesTheSpeaker(t *testing.T) {
	th := tui.NewTheme(tui.Caps{})
	cases := []struct {
		role string
		want string
	}{
		{role: "user", want: "user"},
		{role: "assistant", want: "assistant"},
		{role: "system_note", want: "system_note"},
		{role: "a role this build has no banner for", want: "a role this build has no banner for"},
	}
	for _, tc := range cases {
		t.Run(tc.role+": banner reads "+tc.want, func(t *testing.T) {
			var buf bytes.Buffer
			RenderTurn(&buf, th, memory.Turn{Role: tc.role, Index: 7})
			assert.Contains(t, buf.String(), tc.want)
			assert.Contains(t, buf.String(), "(turn 7)", "the turn index is part of the banner")
		})
	}
}
