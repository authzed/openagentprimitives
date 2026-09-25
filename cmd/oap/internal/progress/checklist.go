package progress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// spinnerInterval is the wall-clock time each spinner frame is shown. The frame
// is derived from time.Now() (not a per-tick counter) so the spin rate is
// constant regardless of how many rows animate concurrently — N parallel waits
// each run their own animate loop, and a shared per-tick counter would advance
// N× too fast (the "spins way too fast" bug with parallel build rows). 100ms
// matches the pace of a single live wait row (e.g. webd gateway address).
const spinnerInterval = 100 * time.Millisecond

type rowStatus int

const (
	statusPending rowStatus = iota
	statusActive
	statusDone
	statusFailed
	statusSkipped
)

type row struct {
	name       string
	status     rowStatus
	label      string    // transient detail, e.g. "waiting (2m)"
	liveStatus string    // last output line from Status(); incorporated by wait-label rendering
	lastPollAt time.Time // wall-clock time of the last readiness check; drives "last checked Xs ago"
}

// Compile-time interface assertions.
var _ Reporter = (*checklist)(nil)
var _ Phase = (*checklistPhase)(nil)
var _ waitSink = (*checklistPhase)(nil)

type checklist struct {
	mu           sync.Mutex // guards all fields below and all writes to out
	out          io.Writer
	stdin        io.Reader
	assumeYes    bool
	width        int // terminal column width; used to clamp rendered rows
	height       int // terminal row count, or 0 when unmeasurable; clamps the live region so a redraw's cursor-up never overruns the pane
	rows         []*row
	linesDrawn   int
	promptActive bool    // true while a keepWaiting prompt owns the screen; suppresses redraw
	fix          FixFunc // installer-supplied AI fixer; nil keeps the prompt at [Y/n]

	// th is resolved once, here, and never replaced. Every state change on
	// every row rewrites the whole live region, so a theme rebuilt per redraw
	// would put a terminal measurement and a full style build in the path of
	// each 100ms spinner tick, times one loop per animating row.
	th *tui.Theme

	// rail is the optional persistent step rail composited to the left of the
	// checklist body (NewWithRail). nil (the default, and the only value New
	// ever produces) keeps redraw on the original single-column path — see
	// redraw's dispatch.
	rail RailProvider
}

// fallbackWidth is the column budget used when out is not a file whose terminal
// can be measured — a test buffer, or a pipe. It matches pkg/cli/tui's own
// fallback, so a row is clamped to the same width whichever path set it.
const fallbackWidth = 80

func newChecklist(out io.Writer, stdin io.Reader, assumeYes bool) *checklist {
	return newChecklistWithTheme(out, stdin, assumeYes, detectTheme(out))
}

// detectTheme resolves the theme newChecklist and NewWithRail both build
// their checklist against: the real terminal capabilities when out is a
// file, else the same colorless fallback width both paths already agreed on.
//
// The --no-color FLAG does not reach this package: progress.New takes no
// such argument, and threading one through its callers is not this change.
// NO_COLOR is still honored, twice over — tui.Detect reads it, and
// cliout.IsTTY reads it before New/NewWithRail ever select this renderer, so
// a NO_COLOR install gets the streaming renderer's plain lines instead.
func detectTheme(out io.Writer) *tui.Theme {
	caps := tui.Caps{Width: fallbackWidth}
	if f, ok := out.(*os.File); ok {
		caps = tui.Detect(f, false)
	}
	return tui.NewTheme(caps)
}

// newChecklistWithTheme builds the renderer against a caller-supplied theme.
//
// It exists so the rendering can be exercised at capabilities the test process
// does not have: a bytes.Buffer is not a file, so newChecklist can only ever
// produce a colorless theme there, and a renderer that quietly stopped
// consulting the theme would look identical.
func newChecklistWithTheme(out io.Writer, stdin io.Reader, assumeYes bool, th *tui.Theme) *checklist {
	width := th.Caps.Width
	if width <= 0 {
		width = fallbackWidth
	}
	// height is left at th.Caps.Height with NO fallback: unlike width, a wrong
	// guess here is worse than none. 0 means "pane height unknown" and disables
	// region clamping (redrawPlain/redrawWithRail draw the full region, as
	// before); a real terminal carries a real row count that clampRegionLines
	// then honors.
	return &checklist{out: out, stdin: stdin, assumeYes: assumeYes, width: width, height: th.Caps.Height, th: th}
}

// newChecklistWithRail is newChecklistWithTheme's theme+rail-injecting
// sibling: it builds the renderer against a caller-supplied theme AND wires
// the optional left rail gutter. It exists for the same reason
// newChecklistWithTheme does — a bytes.Buffer can't produce a themeable file,
// so the rail composite needs a seam the test process can drive directly —
// and it is also NewWithRail's real constructor on a TTY.
func newChecklistWithRail(out io.Writer, stdin io.Reader, assumeYes bool, th *tui.Theme, rail RailProvider) *checklist {
	c := newChecklistWithTheme(out, stdin, assumeYes, th)
	c.rail = rail
	return c
}

func (c *checklist) Phase(name string) Phase {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := &row{name: name, status: statusActive}
	c.rows = append(c.rows, r)
	c.redraw()
	return &checklistPhase{c: c, row: r}
}

func (c *checklist) Info(f string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printAbove(fmt.Sprintf(f, a...))
}
func (c *checklist) OK(f string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printAbove(c.th.Render(c.th.Success, fmt.Sprintf(f, a...)))
}
func (c *checklist) Warn(f string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printAbove(c.th.Render(c.th.Warn, "warning: "+fmt.Sprintf(f, a...)))
}

// SetFixHook installs the AI-fixer hook used by the keep-waiting prompt.
func (c *checklist) SetFixHook(fn FixFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fix = fn
}

func (c *checklist) Close() error {
	// Leave the final checklist on screen; emit a trailing newline so the
	// shell prompt appears on its own line.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.linesDrawn > 0 {
		fmt.Fprint(c.out, "\n")
	}
	return nil
}

// glyph renders a row's status marker.
//
// The three step states take pkg/cli/tui's RAIL styles, not a parallel set: an
// install checklist is a step rail — one row per component, each pending, then
// being worked on, then answered — so it is the same vocabulary the wizard's
// rail draws, and `oap install` and `oap channel create` mark a finished step the
// same way. Failed and skipped have no rail counterpart (a wizard step cannot
// fail), so they take the status colors.
func (c *checklist) glyph(r *row) string {
	switch r.status {
	case statusActive:
		frame := int(time.Now().UnixNano()/int64(spinnerInterval)) % len(spinnerFrames)
		return c.th.Render(c.th.RailActive, string(spinnerFrames[frame]))
	case statusDone:
		return c.th.Render(c.th.RailDone, "✓")
	case statusFailed:
		return c.th.Render(c.th.Err, "✗")
	case statusSkipped:
		return c.th.Render(c.th.Warn, "⊘")
	default:
		return c.th.Render(c.th.Rail, "○")
	}
}

// renderRow renders r at the checklist's full column width. It is the
// no-rail entry point (redrawPlain, and the tests that call it directly);
// renderRowWidth is the shared implementation the rail composite also uses
// at a narrowed width.
func (c *checklist) renderRow(r *row) string {
	return c.renderRowWidth(r, c.width)
}

// renderRowWidth renders r clamped to width columns instead of c.width, so
// the rail composite (redrawWithRail) can budget the body column at
// c.width minus the rail column and gutter without a second copy of the
// truncation logic.
func (c *checklist) renderRowWidth(r *row, width int) string {
	prefix := fmt.Sprintf("%s %s", c.glyph(r), r.name)
	if r.label == "" {
		return prefix
	}
	// Sanitize before measuring: the label is arbitrary text (a captured docker
	// build line via Status), and a tab, a wide rune, or an embedded ANSI escape
	// each defeat a rune-count clamp — the physical line overflows and wraps, but
	// linesDrawn keeps counting logical lines, so every later redraw is offset by
	// the wrap count and the region smears. sanitizeLabel makes every remaining
	// glyph occupy the cells lipgloss.Width counts.
	label := sanitizeLabel(r.label)
	budget := width - lipgloss.Width(prefix) - 2 // 2 = the "  " separator
	if budget < 1 {
		return prefix
	}
	// Truncate by DISPLAY width, not rune count, so a wide rune counts as its two
	// cells. The label carries no escapes at this point (sanitizeLabel stripped
	// them), so ansi.Truncate cannot bisect one — the failure a raw rune slice hit.
	label = ansi.Truncate(label, budget, "…")
	return prefix + "  " + c.th.Render(c.th.Subtle, label)
}

// sanitizeLabel makes an arbitrary status/detail string safe to place on a
// single live-region line. It strips ANSI escape sequences FIRST (they render
// as zero cells and, cut mid-sequence by truncation, would swallow following
// bytes — including the next redraw's cursor-up escape), then replaces every
// remaining C0 control character — tab, newline, carriage return, DEL, and any
// stray lone ESC — with a space so no glyph occupies more or fewer cells than
// lipgloss.Width measures. Order matters: replacing controls first would shatter
// each escape's leading ESC into visible "[31m" debris instead of removing it.
func sanitizeLabel(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// redraw rewrites the live region in place. Caller must hold c.mu. When
// promptActive is set, redraw is a no-op so that a concurrent keepWaiting
// prompt owns the terminal and other goroutines' state updates (row labels,
// status transitions) accumulate silently until the prompt resolves and
// promptActive is cleared.
//
// It dispatches on c.rail: nil takes redrawPlain, the original single-column
// path, UNCHANGED — every non-wizard caller (oap install, oap init) goes
// through New, which never sets c.rail, so that path's output is provably
// identical to before this method grew a rail. A non-nil rail takes
// redrawWithRail, the composite path.
func (c *checklist) redraw() {
	if c.promptActive {
		return // a keepWaiting prompt owns the screen; skip write, preserve state
	}
	if c.rail == nil {
		c.redrawPlain()
		return
	}
	c.redrawWithRail()
}

// clampRegionLines clamps a rendered live region to the terminal height. A
// region taller than the pane cannot be rewritten in place: the cursor-up
// escape (\033[NA) is itself clamped by the terminal to the top of the screen,
// so a redraw with N greater than the pane height repaints from the wrong row
// and stacks duplicates into scrollback at tick rate. When the pane height is
// known and the region would exceed height-1 lines (one row is reserved so the
// cursor-up never has to reach the very top), keep only the LAST height-1 lines.
// The dropped leading lines are NOT preserved above the region: the next redraw
// overwrites the old top line in place, so the oldest rows are erased from the
// pane, not scrolled into history. Losing sight of the oldest Done rows is the
// intended trade — it beats a cursor-up that overruns the pane and smears
// duplicates at tick rate. height <= 0 (a pipe, a test buffer, a width-only
// fallback) disables clamping and the region draws in full, exactly as before —
// which is what keeps the no-rail path byte-identical off a real terminal.
func (c *checklist) clampRegionLines(lines []string) []string {
	if c.height <= 0 {
		return lines
	}
	max := c.height - 1
	if max < 1 {
		max = 1
	}
	if len(lines) > max {
		return lines[len(lines)-max:]
	}
	return lines
}

// redrawPlain rewrites the live region (bottom len(rows) lines) in place — the
// original single-column path. Off a measurable terminal (height <= 0) it is
// byte-identical to what it produced before NewWithRail existed: the lines
// slice is drawn one-per-row exactly as the old row loop did, and
// clampRegionLines is a no-op. On a real terminal shorter than the region, the
// clamp keeps the cursor-up within the pane.
func (c *checklist) redrawPlain() {
	lines := make([]string, len(c.rows))
	for i, r := range c.rows {
		lines[i] = c.renderRow(r)
	}
	lines = c.clampRegionLines(lines)
	if c.linesDrawn > 0 {
		fmt.Fprintf(c.out, "\033[%dA", c.linesDrawn) // cursor up to region top
	}
	for _, ln := range lines {
		fmt.Fprint(c.out, "\r\033[K") // clear the line
		fmt.Fprintln(c.out, ln)
	}
	c.linesDrawn = len(lines)
}

// redrawWithRail rewrites the live region as a composite block: the wizard's
// persistent step rail (c.rail) joined beside the checklist body. c.rail is
// read fresh here — Steps() and Active() — so advancing the wizard's active
// phase repaints the rail on the very next tick, the same way a row status
// change repaints the body.
func (c *checklist) redrawWithRail() {
	// Mirror redrawPlain's zero-rows semantics: before the first Phase row
	// exists there is nothing to composite the rail against, so draw NOTHING and
	// leave linesDrawn at 0 — pre-Phase Info lines then stream plainly through
	// printAbove, exactly as the no-rail path does. Without this the rail block
	// is ~11 lines tall from the first Info (before any Phase), and every direct
	// write to out in that window — an ensure body, a mid-pipeline prompt —
	// desyncs the cursor from linesDrawn and corrupts the region on the next
	// redraw. The live region turns on at the first Phase, not the first Info.
	if len(c.rows) == 0 {
		return
	}
	// Too narrow for a rail column beside a usable body: draw body-only, exactly
	// as tui.Chrome drops its rail below the same width. Compositing here would
	// force compositeBlock's bodyWidth to its 1-cell floor, so the joined line
	// would exceed c.width and wrap — undercounting linesDrawn and smearing the
	// region at tick rate. c.width is fixed for this checklist's lifetime, so this
	// resolves the same way on every redraw (no plain/composite flip-flop).
	if !c.railFits() {
		c.redrawPlain()
		return
	}
	block := c.compositeBlock()
	lines := c.clampRegionLines(strings.Split(block, "\n"))
	prev := c.linesDrawn
	if c.linesDrawn > 0 {
		fmt.Fprintf(c.out, "\033[%dA", c.linesDrawn) // cursor up to region top
	}
	for _, ln := range lines {
		fmt.Fprint(c.out, "\r\033[K") // clear the line
		fmt.Fprintln(c.out, ln)
	}
	// Unlike the plain path (height = len(rows), monotonic), the composite's
	// height is max(len(steps), bodyHeight), and bodyHeight drops back when a
	// lipgloss-wrapped body line un-wraps. When the new block is shorter than the
	// last one, the cursor now sits just below it with stale rows from the taller
	// previous block still on screen — clear from here to end of screen so they
	// don't linger as ghost rows.
	if len(lines) < prev {
		fmt.Fprint(c.out, "\033[J")
	}
	c.linesDrawn = len(lines)
}

// railDoneMark, railActiveMark and railPendingMark are the rail glyphs for a
// step before/at/after c.rail.Active(). They mirror tui.Chrome's own
// doneMark/activeMark/pendingMark exactly (same runes) so the wizard's own
// rail and this composited copy of it read as one visual system — the two
// packages don't share the constants because Chrome's are unexported and
// progress has no reason to import wizard-only internals for three runes.
const (
	railDoneMark    = "✓"
	railActiveMark  = "▸"
	railPendingMark = " "
)

// railMinWidth keeps the rail column readable when every step label is
// short (the wizard's rail is typically 3-4 short labels: Build, Install,
// Status), so the column doesn't flap narrower/wider across screens. Mirrors
// tui.Chrome's railMinWidth value for the same reason as the glyph constants
// above.
const railMinWidth = 14

// railGutter is the blank column count between the rail and the checklist
// body, matching tui.Chrome's own gutter so the two rails feel like one
// system when the wizard and the checklist are composited on screen together.
const railGutter = 3

// railMinBodyWidth and railNarrowWidth mirror tui.Chrome's minBodyWidth and
// narrowWidth: below them the composite drops the rail and draws body-only
// (redrawPlain), because a rail column beside a usable body column no longer
// fits and the joined line would exceed c.width, physically wrap, and desync
// linesDrawn from the cursor — the same width-overflow smear renderRowWidth
// guards a single row against. Same values as tui.Chrome so the wizard's own
// rail and this composited copy of it vanish at the same terminal widths.
const railMinBodyWidth = 20
const railNarrowWidth = 40

// railColumn renders c.rail as a lipgloss block: one line per step, styled
// by position relative to rail.Active() using the checklist's own theme
// (c.th.Rail/RailActive/RailDone — the same rail vocabulary renderRow's
// glyph() already draws from for individual rows). It returns the rendered,
// width-padded column and that width, so the caller can budget the body
// column against it. Caller must hold c.mu; c.rail must be non-nil.
func (c *checklist) railColumn() (col string, width int) {
	steps := c.rail.Steps()
	active := c.rail.Active()
	// An out-of-range active index (a caller bug, not a user mistake) must not
	// partially mark steps done — normalize it to a sentinel that satisfies
	// neither branch below, so every step falls through to pending rather than
	// a too-large active marking all of them "done". Mirrors tui.Chrome.Render's
	// own defensive normalization for the same failure shape.
	if active < 0 || active >= len(steps) {
		active = -1
	}

	width = c.railColumnWidth()

	var b strings.Builder
	for i, s := range steps {
		switch {
		case i < active:
			b.WriteString(c.th.Render(c.th.RailDone, railDoneMark+" "+s.Label))
		case i == active:
			b.WriteString(c.th.Render(c.th.RailActive, railActiveMark+" "+s.Label))
		default:
			b.WriteString(c.th.Render(c.th.Rail, railPendingMark+" "+s.Label))
		}
		if i < len(steps)-1 {
			b.WriteString("\n")
		}
	}
	return lipgloss.NewStyle().Width(width).Render(b.String()), width
}

// railColumnWidth is the rail column width the step labels demand: railMinWidth,
// or wider when a label plus its glyph and space exceeds it. Factored out of
// railColumn so railFits can weigh the rail against the terminal width without
// rendering the whole column. Caller must hold c.mu; c.rail must be non-nil.
func (c *checklist) railColumnWidth() int {
	width := railMinWidth
	for _, s := range c.rail.Steps() {
		if n := lipgloss.Width(s.Label) + 2; n > width { // +2 = glyph + space
			width = n
		}
	}
	return width
}

// railFits reports whether the step rail and a usable body column both fit at
// the checklist's terminal width — the composite's counterpart to
// tui.Chrome.showsRail. False when the terminal is narrower than railNarrowWidth
// outright, or when the rail column a long label demands (plus its gutter and a
// minimum usable body column) would not fit even in a wider terminal; in either
// case redrawWithRail falls back to redrawPlain so no composited line overflows
// c.width and wraps. c.width is measured once at construction, so this is a
// constant for a given checklist and the plain/composite choice never
// flip-flops mid-run. Caller must hold c.mu; c.rail must be non-nil.
func (c *checklist) railFits() bool {
	if c.width < railNarrowWidth {
		return false
	}
	return c.railColumnWidth()+railGutter+railMinBodyWidth <= c.width
}

// compositeBlock builds the rail+body live region for redrawWithRail:
// c.rail's column joined beside the checklist rows, each row's own width
// budget narrowed by the rail column and its gutter so no composited line
// ever exceeds c.width. Caller must hold c.mu; c.rail must be non-nil.
func (c *checklist) compositeBlock() string {
	railCol, railWidth := c.railColumn()

	bodyWidth := c.width - railWidth - railGutter
	if bodyWidth < 1 {
		bodyWidth = 1
	}
	bodyLines := make([]string, len(c.rows))
	for i, r := range c.rows {
		bodyLines[i] = c.renderRowWidth(r, bodyWidth)
	}
	bodyCol := lipgloss.NewStyle().Width(bodyWidth).Render(strings.Join(bodyLines, "\n"))

	// Render the railGutter as a real blank spacer column: bodyWidth already had
	// railGutter cells subtracted for it above, so without an actual gutter
	// column that reserved width just vanished and the rail sat only railMinWidth
	// slack from the body. Spending it here keeps this composite consistent with
	// tui.Chrome.Render, which reserves and renders the identical gutter.
	gutterCol := lipgloss.NewStyle().Width(railGutter).Render("")

	return lipgloss.JoinHorizontal(lipgloss.Top, railCol, gutterCol, bodyCol)
}

// printAbove clears the live region, prints persistent lines (which scroll into
// history), then redraws the region beneath them.
// Caller must hold c.mu.
func (c *checklist) printAbove(lines ...string) {
	if c.linesDrawn > 0 {
		fmt.Fprintf(c.out, "\033[%dA\r\033[J", c.linesDrawn) // up to top, clear to end of screen
		c.linesDrawn = 0
	}
	for _, ln := range lines {
		fmt.Fprintln(c.out, ln)
	}
	c.redraw()
}

type checklistPhase struct {
	c   *checklist
	row *row
}

func (p *checklistPhase) Detail(f string, a ...any) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.row.label = fmt.Sprintf(f, a...)
	p.c.redraw()
}

// Progress renders a determinate bar into this row's label and redraws. The bar
// string is plain (no embedded ANSI) so renderRow's rune-based width clamp keeps
// the row within the terminal width. A later Detail/Await/Done call replaces the
// bar naturally (the wait spinner overwrites the label; Done clears it).
func (p *checklistPhase) Progress(current, total int, detail string) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.row.label = renderBar(current, total, detail)
	p.c.redraw()
}

func (p *checklistPhase) Done() { p.DoneWith("") }

// DoneWith marks the row done and sets its trailing label to note (empty clears
// it, exactly Done()). Used for the "already running" re-run indicator.
func (p *checklistPhase) DoneWith(note string) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.row.status = statusDone
	p.row.label = note
	p.c.redraw()
}

func (p *checklistPhase) Fail() {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.row.status = statusFailed
	p.c.redraw()
}

func (p *checklistPhase) Await(ctx, recheckBase context.Context, deadline, eta time.Duration, poll Poll, diagnose Diagnose) error {
	return awaitLoop(ctx, recheckBase, p, p.row.name, deadline, poll, diagnose, false, eta)
}

func (p *checklistPhase) AwaitOptional(ctx, recheckBase context.Context, deadline, eta time.Duration, poll Poll, diagnose Diagnose) error {
	return awaitLoop(ctx, recheckBase, p, p.row.name, deadline, poll, diagnose, true, eta)
}

func (p *checklistPhase) Skip(reason string) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.row.status = statusSkipped
	p.row.label = reason
	p.c.redraw()
}

// Status sets a live status string on the row. The next tick or onPoll call
// incorporates it into the wait label as "<status>  (<elapsed> · ~<eta>)".
// An empty string reverts the label to the generic "waiting (...)" prefix.
// This method is safe to call from any goroutine; it takes the checklist
// mutex and does not trigger a redraw — the 100ms tick rate is fast enough
// to pick up the change within one frame.
func (p *checklistPhase) Status(s string) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.row.liveStatus = s
}

// waitSink implementation for *checklistPhase.

func (p *checklistPhase) onPoll(_ string, elapsed, _, eta time.Duration) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	// A readiness check just completed: stamp it so the row can show how long ago.
	p.row.lastPollAt = time.Now()
	p.row.label = buildWaitLabel(p.row.liveStatus, elapsed, eta, 0)
	p.c.redraw()
}

func (p *checklistPhase) tick(_ string, elapsed, _, eta time.Duration) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	// The spinner frame is derived from wall-clock time in glyph(), not advanced
	// here, so the spin rate stays constant no matter how many rows tick.
	sinceCheck := time.Duration(-1) // sentinel: no check has completed yet
	if !p.row.lastPollAt.IsZero() {
		sinceCheck = time.Since(p.row.lastPollAt)
	}
	p.row.label = buildWaitLabel(p.row.liveStatus, elapsed, eta, sinceCheck)
	p.c.redraw()
}

// buildWaitLabel builds the row label for a live wait. When status is
// non-empty it is rendered as "<status>  (<elapsed> · ~<eta>)" so the current
// build step is visible at a glance. When status is empty the prefix instead
// reports when the readiness check last ran ("last checked 8s ago") so a long
// poll (e.g. the ~7m webd gateway address) is visibly alive and re-checking
// rather than a frozen "waiting". The timing suffix is always present so
// operators see total elapsed + ETA. sinceCheck < 0 means no check has
// completed yet. The label is stored verbatim in row.label; renderRow's
// rune-based width clamp truncates it if it exceeds the terminal width.
func buildWaitLabel(status string, elapsed, eta, sinceCheck time.Duration) string {
	timing := waitTimingStr(elapsed, eta)
	if status != "" {
		return fmt.Sprintf("%s  (%s)", status, timing)
	}
	return fmt.Sprintf("%s (%s)", lastCheckedPhrase(sinceCheck), timing)
}

// lastCheckedPhrase describes how long ago the readiness check last ran.
// Negative sinceCheck (no check completed yet) falls back to "waiting"; under
// a second reads "just checked"; otherwise "last checked <d> ago".
func lastCheckedPhrase(sinceCheck time.Duration) string {
	switch {
	case sinceCheck < 0:
		return "waiting"
	case sinceCheck < time.Second:
		return "just checked"
	default:
		return fmt.Sprintf("last checked %s ago", sinceCheck.Round(time.Second))
	}
}

// waitTimingStr formats the elapsed + ETA portion of a wait label.
// When eta > 0 it returns "<elapsed> · ~<eta>"; otherwise just "<elapsed>".
func waitTimingStr(elapsed, eta time.Duration) string {
	if eta > 0 {
		return fmt.Sprintf("%s · ~%s", elapsed.Round(time.Second), humanETA(eta))
	}
	return elapsed.Round(time.Second).String()
}

// barCells is the unicode progress-bar width in terminal cells.
const barCells = 16

// renderBar builds a plain (un-styled) determinate progress bar of the form
// "[████████░░░░░░░░] 12/20 detail". Inputs are clamped so 0 ≤ current ≤ total
// and a zero total renders an empty bar rather than dividing by zero. The string
// contains only rune-width-1 glyphs and no ANSI escapes, so the caller's width
// clamp (renderRow) can truncate it safely.
func renderBar(current, total int, detail string) string {
	if total < 0 {
		total = 0
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	filled := 0
	if total > 0 {
		filled = current * barCells / total
	}
	if filled > barCells {
		filled = barCells
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barCells-filled)
	s := fmt.Sprintf("[%s] %d/%d", bar, current, total)
	if detail != "" {
		s += " " + detail
	}
	return s
}

func (p *checklistPhase) animated() bool { return true }

func (p *checklistPhase) warn(f string, a ...any) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.c.printAbove(p.c.th.Render(p.c.th.Warn, "warning: "+fmt.Sprintf(f, a...)))
}

func (p *checklistPhase) renderDiagnosis(d wait.Diagnosis) {
	// Build the output lines outside the lock to avoid holding it during
	// potentially expensive string formatting.
	var buf bytes.Buffer
	writeDiagnosis(&buf, d)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.c.printAbove(lines...)
}

// interactive returns true only when the checklist is on a real TTY and
// --yes was not supplied — the same condition that gate-selected the checklist
// renderer in the first place.
func (p *checklistPhase) interactive() bool { return !p.c.assumeYes }

// fixHook returns the installer-supplied AI fixer (nil keeps the prompt at [Y/n]).
func (p *checklistPhase) fixHook() FixFunc {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return p.c.fix
}

// Suspend runs fn with exclusive ownership of the terminal: it serializes against
// other prompts via promptMu, sets promptActive (so concurrent animate ticks'
// redraws become no-ops), clears the live region, then runs fn — passing the
// renderer's writer — with NO lock held so fn can block on stdin or stream a
// subprocess. When fn returns, the region is redrawn.
//
// Lock discipline:
//  1. Acquire promptMu (package-level) so concurrent phases queue, not interleave.
//  2. Acquire c.mu, set promptActive, clear the region, release c.mu.
//  3. Run fn (blocking stdin read / subprocess stream) without holding any lock.
//  4. Acquire c.mu, clear promptActive, redraw, release c.mu.
//  5. Release promptMu (via defer).
func (c *checklist) Suspend(fn func(out io.Writer, in io.Reader)) {
	promptMu.Lock()
	defer promptMu.Unlock()

	c.mu.Lock()
	c.promptActive = true
	if c.linesDrawn > 0 {
		fmt.Fprintf(c.out, "\033[%dA\r\033[J", c.linesDrawn)
		c.linesDrawn = 0
	}
	out := c.out
	in := c.stdin
	c.mu.Unlock()

	fn(out, in)

	c.mu.Lock()
	c.promptActive = false
	c.redraw()
	c.mu.Unlock()
}

// keepWaiting prompts the user with the live region quiesced. When onFix is
// non-nil the prompt offers [Y/n/f]; the AI session runs while the region is
// suspended so the launched CLI owns the terminal. The quiesce/clear/redraw
// discipline lives in Suspend — keepWaiting just supplies the prompt body.
func (p *checklistPhase) keepWaiting(name string, onFix func() error) bool {
	var ok bool
	p.c.Suspend(func(out io.Writer, in io.Reader) {
		ok = confirmKeepWaiting(in, out, name, onFix)
	})
	return ok
}
