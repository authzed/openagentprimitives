package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hasANSI reports whether s carries any escape sequence. The whole point of
// color-disabled mode is that piped/CI output is byte-clean.
func hasANSI(s string) bool { return strings.Contains(s, "\x1b[") }

func TestNewThemeEmitsNoANSIWhenColorDisabled(t *testing.T) {
	th := NewTheme(Caps{TTY: false, Color: false, Width: 80})
	require.NotNil(t, th.Form, "a huh theme must always be produced, even uncolored")

	cases := []struct {
		name  string
		style lipgloss.Style
	}{
		{"Title", th.Title},
		{"Subtle", th.Subtle},
		{"Success", th.Success},
		{"Warn", th.Warn},
		{"Err", th.Err},
		{"Label", th.Label},
		{"Value", th.Value},
		{"RoleUser", th.RoleUser},
		{"RoleAssistant", th.RoleAssistant},
		{"RoleSystem", th.RoleSystem},
		{"RoleTool", th.RoleTool},
	}
	for _, tc := range cases {
		t.Run(tc.name+": renders plain text with no escape sequences", func(t *testing.T) {
			out := th.Render(tc.style, "hello")
			assert.Equal(t, "hello", out)
			assert.False(t, hasANSI(out))
		})
	}
}

func TestNewThemeEmitsANSIWhenColorEnabled(t *testing.T) {
	th := NewTheme(Caps{TTY: true, Color: true, Width: 100})
	out := th.Render(th.Title, "hello")
	assert.True(t, hasANSI(out), "color-enabled theme must emit escape sequences")
	assert.Contains(t, out, "hello", "styling must not mangle the text")
}

// TestNewThemeGivesEachSpeakerItsOwnColor pins what the four role styles are
// FOR: a reader scanning a transcript tells "user" from "assistant" from
// "system_note" from "tool" by color alone, because every banner is otherwise
// the same bold word on its own line.
//
// Foregrounds are compared pairwise rather than against literal ANSI indices,
// so the palette stays free to move: the claim is that no two speakers collide,
// not that a person's turn is specifically blue.
func TestNewThemeGivesEachSpeakerItsOwnColor(t *testing.T) {
	th := NewTheme(Caps{TTY: true, Color: true, Width: 80})

	roles := map[string]lipgloss.Style{
		"RoleUser":      th.RoleUser,
		"RoleAssistant": th.RoleAssistant,
		"RoleSystem":    th.RoleSystem,
		"RoleTool":      th.RoleTool,
	}
	seen := map[string]string{} // foreground → the role that claimed it
	for name, style := range roles {
		assert.True(t, style.GetBold(), "%s is a banner introducing a block, so it is bold", name)
		fg := fmt.Sprintf("%v", style.GetForeground())
		if prev, dup := seen[fg]; dup {
			assert.Failf(t, "two speakers share one color",
				"%s and %s both render %s, so a transcript cannot be read by color", prev, name, fg)
		}
		seen[fg] = name
	}
}

func TestNewThemeBlurredFieldTitleDiffersFromFocused(t *testing.T) {
	th := NewTheme(Caps{TTY: true, Color: true, Width: 80})

	// Compare the style CONFIGURATION rather than rendered bytes: huh.Theme
	// styles (unlike Theme's own Title/Subtle/etc.) bind to lipgloss's
	// DEFAULT renderer, whose color profile depends on auto-detecting a real
	// terminal — under `go test` that's not one, so a rendered-bytes
	// comparison would pass or fail based on the test environment rather
	// than on what NewTheme actually configured.
	focused := th.Form.Focused.Title
	blurred := th.Form.Blurred.Title

	// A multi-field group renders every field's title through one of these
	// two styles at once. If they carry the same accent color and weight,
	// every field looks focused simultaneously and a user can't tell which
	// one they're answering.
	assert.True(t, focused.GetBold(), "sanity: the focused title is bold")
	assert.False(t, blurred.GetBold(), "a blurred field's title must not be bold like the focused one")
	assert.NotEqual(t, focused.GetForeground(), blurred.GetForeground(),
		"a blurred field's title must not share the focused field's accent color")
}

func TestNewThemeBlurredFieldErrorIndicatorStaysRed(t *testing.T) {
	th := NewTheme(Caps{TTY: true, Color: true, Width: 80})

	// Unlike Title (a focus cue that must NOT persist onto a blurred field),
	// an error indicator is a fact about the field's value regardless of
	// which field the user is currently on — a user tabbing away from an
	// invalid field must not see its error marker go color-quiet.
	assert.Equal(t, th.Form.Focused.ErrorIndicator.GetForeground(), th.Form.Blurred.ErrorIndicator.GetForeground(),
		"a blurred field's error indicator must stay the same red as a focused field's")
	assert.Equal(t, th.Form.Focused.ErrorMessage.GetForeground(), th.Form.Blurred.ErrorMessage.GetForeground(),
		"a blurred field's error message must stay the same red as a focused field's")
}

func TestNewThemeCarriesCaps(t *testing.T) {
	c := Caps{TTY: true, Color: true, Width: 123}
	assert.Equal(t, c, NewTheme(c).Caps, "Theme carries Caps so drivers need only one argument")
}

// TestThemeMarkersAreASCII guards the fix for the multiselect ghosting bug:
// huh's base theme marks a selected row with • (U+2022) and pages a long option
// list with → / ← (U+2192, U+2190), all East-Asian *ambiguous-width* glyphs.
// Terminals that render those two cells wide — while bubbletea v1.3.x's
// renderer counts them as one — desync the repaint and leave a ghost copy of
// every line the cursor moves off.
//
// Asserted over BOTH capability paths. The colored path is where a wizard
// actually runs, but NewTheme returns early for a colorless one, and that early
// return used to hand back huh's glyphs untouched — so a `--no-color` run on a
// wide-glyph terminal ghosted exactly as before.
func TestThemeMarkersAreASCII(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps Caps
	}{
		{name: "colored", caps: Caps{TTY: true, Color: true, Width: 80}},
		{name: "colorless, which returns early and once skipped the swap", caps: Caps{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			theme := NewTheme(tc.caps).Form

			markers := map[string]string{
				"Focused.SelectedPrefix":      theme.Focused.SelectedPrefix.Value(),
				"Focused.UnselectedPrefix":    theme.Focused.UnselectedPrefix.Value(),
				"Focused.SelectSelector":      theme.Focused.SelectSelector.Value(),
				"Focused.MultiSelectSelector": theme.Focused.MultiSelectSelector.Value(),
				"Focused.NextIndicator":       theme.Focused.NextIndicator.Value(),
				"Focused.PrevIndicator":       theme.Focused.PrevIndicator.Value(),
				"Blurred.SelectedPrefix":      theme.Blurred.SelectedPrefix.Value(),
				"Blurred.UnselectedPrefix":    theme.Blurred.UnselectedPrefix.Value(),
				"Blurred.SelectSelector":      theme.Blurred.SelectSelector.Value(),
				"Blurred.MultiSelectSelector": theme.Blurred.MultiSelectSelector.Value(),
				"Blurred.NextIndicator":       theme.Blurred.NextIndicator.Value(),
				"Blurred.PrevIndicator":       theme.Blurred.PrevIndicator.Value(),
			}
			for name, s := range markers {
				for _, r := range s {
					assert.Lessf(t, r, rune(0x80),
						"%s contains non-ASCII rune %q (U+%04X); ambiguous/wide-width glyphs desync the bubbletea v1.x renderer and ghost lines on cursor move",
						name, r, r)
				}
			}

			// The selected/unselected prefixes must also stay equal display
			// width so option labels line up.
			assert.Equal(t,
				len(theme.Focused.SelectedPrefix.Value()),
				len(theme.Focused.UnselectedPrefix.Value()),
				"selected/unselected prefixes must be the same width to keep labels aligned")
		})
	}
}

// TestFrameDrawsItsBorderAtBothColorCapabilities pins the one style that must
// NOT go quiet when color is off.
//
// Frame is deliberately outside Render's "return the input unchanged" rule: a
// pane border is structure, and a chat window whose box vanished down a pipe
// would be unreadable rather than merely uncolored. Both corners are asserted
// together because the pair is the claim — the border survives, the color does
// not.
func TestFrameDrawsItsBorderAtBothColorCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      Caps
		wantANSI  bool
		assertMsg string
	}{
		{
			name:      "color off: border runes present, no escape codes",
			caps:      Caps{Width: 80},
			wantANSI:  false,
			assertMsg: "a colorless frame must be byte-clean",
		},
		{
			name:      "color on: border runes present, escape codes present",
			caps:      Caps{TTY: true, Color: true, Width: 80},
			wantANSI:  true,
			assertMsg: "a colored frame must carry its border color",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := NewTheme(tc.caps).Frame.Width(10).Render("hi")

			assert.Contains(t, out, "╭", "the frame's top-left corner must be drawn at every capability")
			assert.Contains(t, out, "╯", "the frame's bottom-right corner must be drawn at every capability")
			assert.Contains(t, out, "hi", "the frame must not swallow its content")
			assert.Equal(t, tc.wantANSI, hasANSI(out), tc.assertMsg+":\n%q", out)
		})
	}
}

// TestSelectedHighlightsOnlyWhenColorIsOn pins the cursor-row style: reverse
// video is still an escape sequence, so it has to follow Caps.Color like every
// foreground does.
func TestSelectedHighlightsOnlyWhenColorIsOn(t *testing.T) {
	plain := NewTheme(Caps{Width: 80}).Selected.Render("row")
	assert.Equal(t, "row", plain, "a colorless Selected must leave the row untouched")

	colored := NewTheme(Caps{TTY: true, Color: true, Width: 80}).Selected.Render("row")
	assert.True(t, hasANSI(colored), "a colored Selected must emit the reverse-video sequence")
	assert.Contains(t, colored, "row", "highlighting must not mangle the row")
}

// TestTruncateCutsToDisplayColumnsAndLeavesFittingInputAlone covers the rule
// long-lived TUIs borrow from Chrome. The wide-rune case is the one that
// matters: a byte- or rune-based cut would let a CJK title overflow by exactly
// the number of double-width cells in it.
func TestTruncateCutsToDisplayColumnsAndLeavesFittingInputAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{name: "already fits: byte-identical", in: "short", width: 20, want: "short"},
		{name: "over-long ASCII: cut to width", in: strings.Repeat("x", 30), width: 10, want: strings.Repeat("x", 10)},
		{name: "wide runes: cut in display columns, not runes", in: strings.Repeat("模", 10), width: 6, want: strings.Repeat("模", 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := Truncate(tc.in, tc.width)
			assert.Equal(t, tc.want, out)
			assert.LessOrEqual(t, lipgloss.Width(out), tc.width, "result must fit the budget")
		})
	}
}

// TestTruncatePreservesStylingOfAnAlreadyRenderedString is what lets a caller
// style a title first and clamp it second — the order both bubbletea TUIs use,
// because their header is assembled from several differently-styled spans
// before its total width is known.
func TestTruncatePreservesStylingOfAnAlreadyRenderedString(t *testing.T) {
	th := NewTheme(Caps{TTY: true, Color: true, Width: 80})
	styled := th.Render(th.Title, strings.Repeat("x", 30))

	out := Truncate(styled, 10)
	assert.True(t, hasANSI(out), "truncation must not strip the styling it was handed")
	assert.LessOrEqual(t, lipgloss.Width(out), 10, "the visible text must still fit the budget")
}
