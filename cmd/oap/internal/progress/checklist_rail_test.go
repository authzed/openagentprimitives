package progress

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// buildSteps returns n placeholder rail steps, for the height-clamp tests that
// only care that the rail makes the composite region tall.
func buildSteps(n int) []tui.Step {
	out := make([]tui.Step, n)
	for i := range out {
		out[i] = tui.Step{ID: fmt.Sprintf("s%d", i), Label: fmt.Sprintf("Step %d", i)}
	}
	return out
}

// fakeRail is a minimal RailProvider: a fixed step list and a mutable active
// index, standing in for the unified init wizard's own step navigation.
type fakeRail struct {
	steps  []tui.Step
	active int
}

func (f *fakeRail) Steps() []tui.Step { return f.steps }
func (f *fakeRail) Active() int       { return f.active }

// TestChecklistRendersRailGutter is Task 9's core claim: a checklist built
// with a RailProvider composites the rail beside its own rows, and both the
// rail's step labels and the checklist's row text land in the same output.
func TestChecklistRendersRailGutter(t *testing.T) {
	var buf bytes.Buffer
	rail := &fakeRail{
		steps: []tui.Step{
			{ID: "build", Label: "Build"},
			{ID: "install", Label: "Install"},
			{ID: "status", Label: "Status"},
		},
		active: 1,
	}
	th := tui.NewTheme(tui.Caps{Width: 80}) // colorless: no *os.File behind buf
	c := newChecklistWithRail(&buf, nil, false, th, rail)

	ph := c.Phase("NATS (message bus)")
	ph.Done()
	c.Phase("PostgreSQL (agent memory)") // active row

	out := buf.String()
	assert.Contains(t, out, "Build", "rail label present")
	assert.Contains(t, out, "Install", "active rail step present")
	assert.Contains(t, out, "NATS (message bus)", "body row present")
	assert.Contains(t, out, "PostgreSQL (agent memory)", "body row present")

	// Glyph vocabulary: the completed body row shows the done mark, and the
	// active rail step shows the active mark — same runes tui.Chrome's own
	// rail draws (railDoneMark/railActiveMark).
	assert.Contains(t, out, "✓", "completed checklist row shows the done glyph")
	assert.Contains(t, out, "▸", "active rail step shows the active glyph")
}

// TestChecklistRail_NilRailIsUnchanged pins the fallback contract: a
// checklist built with a nil RailProvider must render identically to one
// built with no rail support at all — no gutter, no composite, byte-for-byte
// the same as newChecklistWithTheme.
//
// Both trailing phases are Done()'d before the byte comparison: the active-row
// glyph is a time-based spinner (checklist.go glyph, statusActive), so leaving a
// phase active lets the two renders straddle a spinner-frame boundary and flake
// the require.Equal. With every row completed, no active glyph participates and
// the comparison is deterministic — the nil-vs-no-rail identity it exists to
// prove is unchanged.
func TestChecklistRail_NilRailIsUnchanged(t *testing.T) {
	th := tui.NewTheme(tui.Caps{Width: 80})

	var withNilRail bytes.Buffer
	c1 := newChecklistWithRail(&withNilRail, nil, false, th, nil)
	c1.Phase("NATS (message bus)").Done()
	c1.Phase("PostgreSQL (agent memory)").Done()

	var withoutRail bytes.Buffer
	c2 := newChecklistWithTheme(&withoutRail, nil, false, th)
	c2.Phase("NATS (message bus)").Done()
	c2.Phase("PostgreSQL (agent memory)").Done()

	require.Equal(t, withoutRail.String(), withNilRail.String(),
		"a nil RailProvider must render byte-identical to the no-rail constructor")
}

// TestChecklistRail_PrePhaseInfoStreamsPlainly is the F1a regression — the core
// of the reported "status updates impacting the TUI in an odd way". Before any
// Phase row exists, a rail-backed checklist must NOT stand up an ~11-line live
// region: pre-Phase Info lines stream plainly (linesDrawn stays 0), exactly as
// the no-rail path does, so a direct-to-out write in that window cannot desync
// the cursor from linesDrawn. The live region turns on at the first Phase, not
// the first Info.
func TestChecklistRail_PrePhaseInfoStreamsPlainly(t *testing.T) {
	rail := &fakeRail{
		steps: []tui.Step{
			{ID: "build", Label: "Build"},
			{ID: "install", Label: "Install"},
			{ID: "status", Label: "Status"},
		},
		active: 0,
	}
	th := tui.NewTheme(tui.Caps{Width: 80})

	var railed bytes.Buffer
	c := newChecklistWithRail(&railed, nil, false, th, rail)
	c.Info("preflight checks")
	c.Info("cluster reachable")

	assert.Equal(t, 0, c.linesDrawn, "no Phase yet: the live region must not exist")
	out := railed.String()
	assert.Contains(t, out, "preflight checks", "pre-Phase Info must still print")
	assert.NotContains(t, out, "Build", "the rail must not draw before the first Phase row")
	assert.NotContains(t, out, "Install", "the rail must not draw before the first Phase row")

	// Byte-identical to the nil-rail path for the same pre-Phase sequence: the
	// rail changes nothing on screen until a Phase row exists to composite it
	// against, so the two outputs cannot diverge here.
	var plain bytes.Buffer
	c2 := newChecklistWithRail(&plain, nil, false, th, nil)
	c2.Info("preflight checks")
	c2.Info("cluster reachable")
	assert.Equal(t, plain.String(), railed.String(),
		"pre-Phase Info must stream identically with or without a rail")

	// And once a Phase row lands, the rail DOES appear — the region turns on at
	// the Phase, proving the guard is scoped to the zero-rows window, not a
	// blanket suppression.
	c.Phase("NATS (message bus)")
	assert.Greater(t, c.linesDrawn, 0, "the live region turns on at the first Phase")
	assert.Contains(t, railed.String(), "Build", "the rail draws once a Phase row exists")
}

// TestRenderRowWidth_SanitizesControlAnsiWideRunes is the F3 unit regression: a
// label carrying a tab, a wide (CJK) rune, an emoji, and an ANSI escape must be
// clamped to the DISPLAY-width budget — a rune-count clamp let each of these
// overflow the physical line and wrap, which then offset every later redraw.
func TestRenderRowWidth_SanitizesControlAnsiWideRunes(t *testing.T) {
	th := tui.NewTheme(tui.Caps{Width: 80}) // colorless: no SGR added by the theme
	c := newChecklistWithTheme(&bytes.Buffer{}, nil, false, th)

	r := &row{
		name:   "build runner",
		status: statusActive,
		label:  "\x1b[31m#5 RUN\tmake 世界🌍 " + strings.Repeat("y", 300) + "\x1b[0m",
	}
	for _, width := range []int{20, 40, 60, 80} {
		t.Run(fmt.Sprintf("width=%d: no overflow, no residual control/ansi", width), func(t *testing.T) {
			got := c.renderRowWidth(r, width)
			assert.LessOrEqual(t, lipgloss.Width(got), width,
				"a sanitized, display-width-truncated row must never exceed the budget:\n%q", got)
			assert.NotContains(t, got, "\t", "tabs must be replaced with a space")
			assert.NotContains(t, got, "\x1b[31m", "ANSI SGR escapes in the label must be stripped")
			assert.NotContains(t, got, "\x1b[0m", "ANSI SGR escapes in the label must be stripped")
		})
	}
}

// TestChecklistRail_NastyStatus_StableHeightNoOverflow is the F3 integration
// regression at the narrowed rail body width: a docker-style status line fed
// through Status() must not overflow any composited line, and the region height
// must stay stable across redraws (an overflow would wrap and grow it per tick).
func TestChecklistRail_NastyStatus_StableHeightNoOverflow(t *testing.T) {
	rail := &fakeRail{steps: []tui.Step{{ID: "build", Label: "Build"}}, active: 0}
	th := tui.NewTheme(tui.Caps{Width: 60})
	var buf bytes.Buffer
	c := newChecklistWithRail(&buf, nil, false, th, rail)

	ph := c.Phase("build runner").(*checklistPhase)
	ph.Status("\x1b[36m#8 12.3\tgo: downloading 世界🌍/mod " + strings.Repeat("x", 200) + "\x1b[0m")
	ph.tick("build runner", 4*time.Second, time.Second, 2*time.Minute)

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 60,
			"no composited line may exceed the terminal width:\n%q", line)
	}
	before := c.linesDrawn
	buf.Reset()
	c.mu.Lock()
	c.redraw()
	c.mu.Unlock()
	assert.Equal(t, before, c.linesDrawn,
		"the region height must not change across redraws of the same rows")
}

// TestRedrawWithRail_ShrinkEmitsClearToEndOfScreen is the F4 regression: when a
// redraw's block is shorter than the last one drawn, the cursor ends just below
// the shorter block with stale rows still on screen; the composite path must
// clear to end of screen so they don't linger as ghost rows. The plain path
// cannot shrink (its height is len(rows), monotonic), so the guard lives only
// on the composite path — this drives that path directly.
func TestRedrawWithRail_ShrinkEmitsClearToEndOfScreen(t *testing.T) {
	rail := &fakeRail{steps: []tui.Step{{ID: "build", Label: "Build"}}, active: 0}
	th := tui.NewTheme(tui.Caps{Width: 80})
	var buf bytes.Buffer
	c := newChecklistWithRail(&buf, nil, false, th, rail)

	c.Phase("a")
	c.Phase("b")
	c.Phase("c")
	require.Equal(t, 3, c.linesDrawn, "precondition: three rows make a three-line composite")

	// Simulate the region shrinking (a previously wrapped body line un-wrapping):
	// drop two rows so the next redraw's block is shorter than the last drawn.
	buf.Reset()
	c.mu.Lock()
	c.rows = c.rows[:1]
	c.redraw()
	c.mu.Unlock()

	assert.Equal(t, 1, c.linesDrawn, "the region shrank to one row")
	assert.Contains(t, buf.String(), "\033[J",
		"a shrunk composite region must clear stale rows to end of screen")
}

// TestChecklistRail_ClampsRegionToPaneHeight is the F7 regression: a live region
// taller than the pane must clamp to height-1, so the redraw's cursor-up never
// exceeds the pane and stacks duplicates into scrollback. The rail's 11 steps
// make the region tall immediately; a 6-row pane forces the clamp.
func TestChecklistRail_ClampsRegionToPaneHeight(t *testing.T) {
	rail := &fakeRail{steps: buildSteps(11), active: 0}
	th := tui.NewTheme(tui.Caps{Width: 80, Height: 6})
	var buf bytes.Buffer
	c := newChecklistWithRail(&buf, nil, false, th, rail)

	c.Phase("only row") // unclamped block is 11 lines (the rail); pane is 6
	require.LessOrEqual(t, c.linesDrawn, 5, "the region must clamp to height-1 on a 6-row pane")
	require.Greater(t, c.linesDrawn, 0, "a clamped region is still drawn, just shorter")

	buf.Reset()
	c.mu.Lock()
	c.redraw()
	c.mu.Unlock()
	frame := buf.String()
	assert.True(t, strings.HasPrefix(frame, fmt.Sprintf("\033[%dA", c.linesDrawn)),
		"the cursor-up must equal the clamped region height, never the unclamped block height:\n%q", frame)
	assert.Equal(t, c.linesDrawn, strings.Count(frame, "\r\033[K"),
		"exactly one clear per drawn line — nothing is drawn beyond the clamp")
	assert.NotContains(t, frame, "\033[11A", "the cursor-up must never exceed the pane height")
}

// TestChecklistRail_NarrowTerminalDropsRail is the NEW-5 regression: below the
// width where a rail column and a usable body column fit side by side, the
// composite must drop the rail and draw body-only (redrawPlain), mirroring
// tui.Chrome.showsRail. Drawing the rail there would force the body to a 1-cell
// floor, overflow the terminal, wrap, and desync linesDrawn. On a narrow pane a
// rail-backed checklist must therefore render byte-identically to a no-rail one.
func TestChecklistRail_NarrowTerminalDropsRail(t *testing.T) {
	rail := &fakeRail{
		steps: []tui.Step{
			{ID: "build", Label: "Build"},
			{ID: "install", Label: "Install"},
			{ID: "status", Label: "Status"},
		},
		active: 1,
	}
	th := tui.NewTheme(tui.Caps{Width: 30}) // below railNarrowWidth (40)

	var railed bytes.Buffer
	c := newChecklistWithRail(&railed, nil, false, th, rail)
	// Both phases Done()'d before the byte comparison: the active-row glyph is a
	// time-based spinner, so a live row could straddle a frame boundary and flake
	// the require.Equal (see TestChecklistRail_NilRailIsUnchanged).
	c.Phase("NATS (message bus)").Done()
	c.Phase("PostgreSQL (agent memory)").Done()

	out := railed.String()
	assert.NotContains(t, out, "Build", "a too-narrow terminal must drop the rail column")
	assert.NotContains(t, out, "Install", "a too-narrow terminal must drop the rail column")
	assert.Contains(t, out, "NATS (message bus)", "the body rows are still drawn")

	// No composited line may exceed the terminal width — the overflow-wrap this
	// fallback exists to prevent.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 30,
			"no line may exceed the narrow terminal width:\n%q", line)
	}

	// Byte-identical to the no-rail constructor at the same width: with the rail
	// dropped, the composite path collapses to exactly redrawPlain's output.
	var plain bytes.Buffer
	c2 := newChecklistWithTheme(&plain, nil, false, th)
	c2.Phase("NATS (message bus)").Done()
	c2.Phase("PostgreSQL (agent memory)").Done()
	assert.Equal(t, plain.String(), railed.String(),
		"below the fittable width a rail-backed checklist renders identically to no-rail")
}

// TestRailFits covers the composite's rail-drop decision directly, mirroring
// tui.Chrome.showsRail: narrower than railNarrowWidth drops the rail outright;
// otherwise the rail fits only when its column, the gutter, and a minimum usable
// body column all fit — a single very long label can push it out even on a wide
// terminal (the hole a narrowWidth-only check cannot see).
func TestRailFits(t *testing.T) {
	short := &fakeRail{steps: []tui.Step{{ID: "b", Label: "Build"}, {ID: "i", Label: "Install"}}}
	longLabel := &fakeRail{steps: []tui.Step{{ID: "b", Label: "Configure the upstream credential source"}}}
	cases := []struct {
		name  string
		rail  *fakeRail
		width int
		want  bool
	}{
		{name: "wide terminal, short labels: rail fits", rail: short, width: 80, want: true},
		{name: "exactly railNarrowWidth with room to spare: rail fits", rail: short, width: railNarrowWidth, want: true},
		{name: "one column below railNarrowWidth: rail dropped", rail: short, width: railNarrowWidth - 1, want: false},
		{name: "long label past a wide-enough terminal: rail dropped", rail: longLabel, width: 50, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &checklist{width: tc.width, rail: tc.rail}
			assert.Equal(t, tc.want, c.railFits())
		})
	}
}

// TestClampRegionLines covers the height-clamp helper directly, including the
// disabled (height <= 0) case that keeps the no-rail path byte-identical off a
// measurable terminal.
func TestClampRegionLines(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	cases := []struct {
		name   string
		height int
		want   []string
	}{
		{name: "height 0 (unknown): no clamp", height: 0, want: lines},
		{name: "negative height: no clamp", height: -1, want: lines},
		{name: "tall pane: region fits, no clamp", height: 10, want: lines},
		{name: "short pane: keep the last height-1 lines", height: 4, want: []string{"c", "d", "e"}},
		{name: "height 1: keep a single last line", height: 1, want: []string{"e"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &checklist{height: tc.height}
			assert.Equal(t, tc.want, c.clampRegionLines(lines))
		})
	}
}
