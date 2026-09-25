package progress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/wait"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

func TestChecklist_RowLifecycle_PendingActiveDone(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	p := c.Phase("postgres")
	p.(*checklistPhase).onPoll("postgres", time.Second, 2*time.Second, 0) // active w/ label
	p.Done()
	require.NoError(t, c.Close())

	out := buf.String()
	assert.Contains(t, out, "postgres", "row label present")
	assert.Contains(t, out, "✓", "done glyph present after Done()")
}

// TestChecklist_Suspend_QuiescesRegionAndPassesWriter is the regression for the
// garbled GKE consent prompt: a prompt emitted while checklist rows are on screen
// must own the terminal exclusively. Suspend clears the live region, sets
// promptActive (so a concurrent animate tick's redraw is a no-op), hands fn the
// renderer's writer, and restores the region afterward.
func TestChecklist_Suspend_QuiescesRegionAndPassesWriter(t *testing.T) {
	var buf bytes.Buffer
	stdin := strings.NewReader("")
	c := newChecklist(&buf, stdin, false)
	c.Phase("NATS") // draws a row; linesDrawn == 1

	const sentinel = "Enable the GKE Gateway API? [y/N]: "
	var sawActive, redrawWasNoOp, gotStdin bool
	c.Suspend(func(out io.Writer, in io.Reader) {
		sawActive = c.promptActive
		gotStdin = in == stdin
		// Simulate a concurrent spinner tick landing mid-prompt. While suspended it
		// must NOT draw a row over the prompt.
		before := buf.Len()
		c.redraw()
		redrawWasNoOp = buf.Len() == before
		fmt.Fprint(out, sentinel) // a prompt has no trailing newline
	})

	out := buf.String()
	assert.True(t, sawActive, "promptActive must be set while the suspended fn runs")
	assert.True(t, gotStdin, "fn must receive the renderer's stdin reader (for subprocess TTY inheritance)")
	assert.True(t, redrawWasNoOp, "a redraw while suspended must be suppressed, never drawing over the prompt")
	assert.False(t, c.promptActive, "promptActive must be cleared after Suspend returns")
	assert.Contains(t, out, "\033[J", "the live region must be cleared before the prompt is written")
	assert.Contains(t, out, sentinel, "fn must receive the renderer's output writer")
	// The region is restored after the prompt resolves.
	assert.Contains(t, out[strings.Index(out, sentinel)+len(sentinel):], "NATS",
		"the checklist region must be redrawn after Suspend returns")
}

func TestChecklist_Warn_PersistsAboveRegion(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	c.Phase("postgres")
	c.Warn("something noteworthy")
	require.NoError(t, c.Close())
	assert.Contains(t, buf.String(), "something noteworthy")
}

func TestChecklist_RenderDiagnosis_IncludesEventText(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	p := c.Phase("postgres").(*checklistPhase)
	p.renderDiagnosis(wait.Diagnosis{
		Pod: "spicebox-postgres-abc", PodPhase: "Pending", Reason: "ContainerCreating",
		Events: []wait.EventNote{{Reason: "FailedAttachVolume", Message: "pd-balanced disk type cannot be used by n4-standard-4", Count: 12}},
	})
	out := buf.String()
	assert.Contains(t, out, "FailedAttachVolume")
	assert.Contains(t, out, "ContainerCreating")
}

func TestChecklist_Fail_ShowsFailGlyph(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	c.Phase("postgres").Fail()
	require.NoError(t, c.Close())
	assert.Contains(t, buf.String(), "✗")
}

func TestChecklist_Skip_ShowsSkippedGlyphAndReason(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	c.Phase("graphiti").Skip("optional; degraded state")
	require.NoError(t, c.Close())
	out := buf.String()
	assert.Contains(t, out, "⊘")
	assert.Contains(t, out, "degraded")
}

// TestChecklist_ConcurrentPhases_RaceFree drives one checklist from several
// goroutines simultaneously, exercising every write path (tick, Detail, Done)
// under -race. This pins the mutex invariant added by Fix 1.
func TestChecklist_ConcurrentPhases_RaceFree(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)

	const n = 4
	phases := make([]Phase, n)
	for i := range phases {
		phases[i] = c.Phase(fmt.Sprintf("phase%d", i))
	}

	var wg sync.WaitGroup
	for _, ph := range phases {
		ph := ph
		wg.Add(1)
		go func() {
			defer wg.Done()
			cp := ph.(*checklistPhase)
			cp.tick("x", time.Second, time.Second, 0)
			cp.Detail("in progress")
			cp.tick("x", 2*time.Second, time.Second, 0)
			ph.Done()
		}()
	}
	wg.Wait()
	require.NoError(t, c.Close())
	assert.NotEmpty(t, buf.String(), "output must be non-empty after concurrent writes")
}

// TestRenderBar_ClampsAndFormats pins the bar-string math: clamping, the empty
// (divide-by-zero-safe) bar, and the optional detail suffix.
func TestRenderBar_ClampsAndFormats(t *testing.T) {
	cases := []struct {
		name           string
		current, total int
		detail         string
		wantContains   []string
		wantFilled     int // count of █ cells
	}{
		{name: "half: 4/8 → 8 filled cells", current: 4, total: 8, wantContains: []string{"4/8"}, wantFilled: 8},
		{name: "overflow current clamps to total / full bar", current: 99, total: 10, wantContains: []string{"10/10"}, wantFilled: barCells},
		{name: "zero total renders empty bar, no divide-by-zero", current: 0, total: 0, wantContains: []string{"0/0"}, wantFilled: 0},
		{name: "negative current clamps to 0 / empty bar", current: -5, total: 4, wantContains: []string{"0/4"}, wantFilled: 0},
		{name: "detail appended after the count", current: 1, total: 1, detail: "1 KB", wantContains: []string{"1/1", "1 KB"}, wantFilled: barCells},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := renderBar(tc.current, tc.total, tc.detail)
			for _, w := range tc.wantContains {
				assert.Contains(t, s, w)
			}
			assert.Equal(t, tc.wantFilled, strings.Count(s, "█"), "filled-cell count")
			assert.Equal(t, barCells, strings.Count(s, "█")+strings.Count(s, "░"), "bar is always barCells wide")
		})
	}
}

// TestChecklist_Progress_RendersBarAndCounts asserts a Progress row renders the
// unicode bar + n/m, and that Done() after current==total flips it to ✓.
func TestChecklist_Progress_RendersBarAndCounts(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	p := c.Phase("postgres")
	p.Progress(3, 8, "")
	p.Progress(8, 8, "")
	p.Done()
	require.NoError(t, c.Close())

	out := buf.String()
	assert.Contains(t, out, "█", "filled bar cell present")
	assert.Contains(t, out, "░", "empty bar cell present at 3/8")
	assert.Contains(t, out, "3/8", "intermediate count present")
	assert.Contains(t, out, "8/8", "final count present")
	assert.Contains(t, out, "✓", "done glyph after Done()")
}

// TestChecklist_Progress_IncludesDetail asserts the optional detail (e.g. a byte
// count) is rendered alongside the bar.
func TestChecklist_Progress_IncludesDetail(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	c.Phase("push").Progress(512, 1024, "512 KB")
	require.NoError(t, c.Close())
	out := buf.String()
	assert.Contains(t, out, "512/1024")
	assert.Contains(t, out, "512 KB")
}

// TestChecklist_Progress_ClampedToWidth asserts an over-long detail on a bar row
// is still clamped to the terminal width by the shared renderRow clamp.
func TestChecklist_Progress_ClampedToWidth(t *testing.T) {
	var buf bytes.Buffer
	// buf is not *os.File so newChecklist defaults width to 80.
	c := newChecklist(&buf, strings.NewReader(""), false)
	c.Phase("push").Progress(1, 2, strings.Repeat("y", 300))
	require.NoError(t, c.Close())
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 80, "no bar row line may exceed the terminal width")
	}
}

// TestChecklist_ConcurrentProgress_RaceFree drives Progress from several
// goroutines on distinct rows simultaneously to pin race-freedom under -race.
func TestChecklist_ConcurrentProgress_RaceFree(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)

	const n = 4
	phases := make([]Phase, n)
	for i := range phases {
		phases[i] = c.Phase(fmt.Sprintf("bar%d", i))
	}

	var wg sync.WaitGroup
	for _, ph := range phases {
		ph := ph
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j <= 20; j++ {
				ph.Progress(j, 20, fmt.Sprintf("%d units", j))
			}
			ph.Done()
		}()
	}
	wg.Wait()
	require.NoError(t, c.Close())
	assert.Contains(t, buf.String(), "20/20", "a completed bar must be rendered")
}

func TestChecklist_LongLabel_ClampedToWidth(t *testing.T) {
	var buf bytes.Buffer
	// buf is not *os.File so newChecklist defaults width to 80.
	c := newChecklist(&buf, strings.NewReader(""), false)
	p := c.Phase("graphiti").(*checklistPhase)
	p.Detail("%s", strings.Repeat("x", 300))
	require.NoError(t, c.Close())
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 80, "no rendered row line may exceed the terminal width")
	}
}

// TestChecklist_PromptActiveQuiesces_Redraw verifies that setting promptActive
// suppresses terminal writes from redraw() while still allowing row state to
// update, and that clearing it and calling redraw() once is enough to flush.
func TestChecklist_PromptActiveQuiesces_Redraw(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	c.Phase("phase1") // initial draw happens here; buf is non-empty
	initial := buf.Len()
	require.Greater(t, initial, 0, "Phase() must produce output")

	// Simulate a prompt in progress: set promptActive.
	c.mu.Lock()
	c.promptActive = true
	c.mu.Unlock()

	// State updates that would normally trigger a redraw must be suppressed.
	c.mu.Lock()
	c.rows[0].label = "some update while prompt is active"
	c.redraw() // must be a no-op
	c.mu.Unlock()

	assert.Equal(t, initial, buf.Len(), "redraw must not write to out while promptActive is true")

	// Clearing promptActive and redrawing must produce output.
	c.mu.Lock()
	c.promptActive = false
	c.redraw()
	c.mu.Unlock()

	assert.Greater(t, buf.Len(), initial, "redraw must write to out after promptActive is cleared")
}

// TestBuildWaitLabel_WithStatus_RendersStatusNotWaiting verifies that
// buildWaitLabel incorporates a non-empty status as the first segment of the
// row label, replacing the generic "waiting" prefix, while still including
// the elapsed/ETA timing in parentheses. An empty status must still produce
// the original "waiting (...)" format so existing behavior is preserved.
func TestBuildWaitLabel_WithStatus_RendersStatusNotWaiting(t *testing.T) {
	cases := []struct {
		name            string
		status          string
		elapsed         time.Duration
		eta             time.Duration
		sinceCheck      time.Duration
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "empty status, no check yet: generic waiting label with elapsed and ETA",
			status:       "",
			elapsed:      3 * time.Second,
			eta:          2 * time.Minute,
			sinceCheck:   -1,
			wantContains: []string{"waiting", "3s", "~2m"},
		},
		{
			name:         "empty status, no ETA: waiting with elapsed only",
			status:       "",
			elapsed:      5 * time.Second,
			eta:          0,
			sinceCheck:   -1,
			wantContains: []string{"waiting", "5s"},
		},
		{
			name:            "empty status, recent check: reports how long ago instead of 'waiting'",
			status:          "",
			elapsed:         2 * time.Minute,
			eta:             7 * time.Minute,
			sinceCheck:      8 * time.Second,
			wantContains:    []string{"last checked 8s ago", "2m", "~7m"},
			wantNotContains: []string{"waiting"},
		},
		{
			name:            "empty status, just polled: 'just checked'",
			status:          "",
			elapsed:         30 * time.Second,
			eta:             7 * time.Minute,
			sinceCheck:      0,
			wantContains:    []string{"just checked", "~7m"},
			wantNotContains: []string{"waiting"},
		},
		{
			name:            "non-empty status: status first, timing in parens, no 'waiting' prefix",
			status:          "#8 12.3 go: downloading example.com/mod v0.1.0",
			elapsed:         5 * time.Second,
			eta:             2 * time.Minute,
			sinceCheck:      3 * time.Second,
			wantContains:    []string{"go: downloading", "5s", "~2m"},
			wantNotContains: []string{"waiting", "checked"},
		},
		{
			name:            "non-empty status, no ETA: status first, elapsed in parens",
			status:          "#3 RUN go build ./...",
			elapsed:         10 * time.Second,
			eta:             0,
			sinceCheck:      3 * time.Second,
			wantContains:    []string{"RUN go build", "10s"},
			wantNotContains: []string{"waiting", "checked"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label := buildWaitLabel(tc.status, tc.elapsed, tc.eta, tc.sinceCheck)
			for _, w := range tc.wantContains {
				assert.Contains(t, label, w, "label must contain %q", w)
			}
			for _, w := range tc.wantNotContains {
				assert.NotContains(t, label, w, "label must not contain %q when status is set", w)
			}
		})
	}
}

// TestChecklist_Status_IntegratesWithRow verifies that Status() stores the
// live status on the row and the next tick renders it in the checklist output.
func TestChecklist_Status_IntegratesWithRow(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	p := c.Phase("build runner").(*checklistPhase)
	p.Status("#5 go: downloading example.com/pkg v1.0.0")
	p.tick("build runner", 4*time.Second, time.Second, 2*time.Minute)
	require.NoError(t, c.Close())
	out := buf.String()
	assert.Contains(t, out, "go: downloading", "status text must appear after tick")
	assert.Contains(t, out, "4s", "elapsed must appear in label")
}

// TestChecklist_ConcurrentKeepWaiting_RaceFree exercises two concurrent
// keepWaiting calls under -race to pin that promptMu serialization is correct
// and that no redraw/prompt output interleaves. Both phases answer "n" so the
// prompts resolve quickly once stdin data is available.
func TestChecklist_ConcurrentKeepWaiting_RaceFree(t *testing.T) {
	// Feed two "n\n" answers. Because prompts are serialized by promptMu and
	// confirmKeepWaiting creates a new bufio.Reader each call, the first call
	// may buffer both lines — the second call then sees EOF and also returns
	// false. Either way neither goroutine blocks indefinitely.
	c := newChecklist(&bytes.Buffer{}, strings.NewReader("n\nn\n"), false)
	p1 := c.Phase("phase1").(*checklistPhase)
	p2 := c.Phase("phase2").(*checklistPhase)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p1.keepWaiting("phase1", nil) }()
	go func() { defer wg.Done(); p2.keepWaiting("phase2", nil) }()
	wg.Wait()
	// Reaching here without a race or deadlock is the assertion.
}

// TestChecklist_KeepWaiting_FInvokesFixerThenReAsks drives the renderer's
// keepWaiting with a fix callback and scripted stdin "f\nn\n": the callback must
// fire once (quiesced behind promptActive) and the prompt re-ask, where 'n'
// stops. Run under -race to pin the quiesce/redraw locking.
func TestChecklist_KeepWaiting_FInvokesFixerThenReAsks(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader("f\nn\n"), false)
	c.SetFixHook(func(context.Context, string, wait.Diagnosis) error { return nil })
	p := c.Phase("postgres").(*checklistPhase)

	var calls int
	onFix := func() error { calls++; return nil }
	ok := p.keepWaiting("postgres", onFix)
	assert.False(t, ok, "scripted 'n' on the re-ask must stop waiting")
	assert.Equal(t, 1, calls, "'f' must invoke the fixer exactly once")
	assert.False(t, c.promptActive, "promptActive must be cleared after the prompt resolves")
	assert.Contains(t, buf.String(), "[Y/n/f]", "prompt must offer the fixer key when a hook is set")
}

// --- theming ---
//
// What these tests can prove: that the rows this renderer composes carry the
// shared palette when the theme has color and nothing but text when it does
// not, and that the live-region rewrite is still one line per row. What they
// cannot prove is what a terminal draws — the checklist's whole mechanism is
// cursor movement, which only a real terminal interprets.

// sgrRE matches an SGR (color/attribute) sequence. It is deliberately NOT "any
// escape sequence": this renderer's cursor-up and clear-line codes are its
// mechanism and are present at every capability, so a blanket \x1b[ ban would be
// a test that could never pass rather than a claim about color.
var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func hasSGR(s string) bool { return sgrRE.MatchString(s) }

// colorTheme is the capability corner newChecklist cannot reach from a test: a
// bytes.Buffer is not a file, so tui.Detect always answers "no color" here.
func colorTheme() *tui.Theme { return tui.NewTheme(tui.Caps{TTY: true, Color: true, Width: 80}) }

// TestChecklist_ColorlessTheme_PlainRowsButLiveRegionIntact is the CI claim: an
// install whose output is not a color-capable terminal must leave no color in
// the log, while still driving the region it redraws.
func TestChecklist_ColorlessTheme_PlainRowsButLiveRegionIntact(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklist(&buf, strings.NewReader(""), false)
	require.False(t, c.th.Caps.Color, "precondition: a non-file writer must yield a colorless theme")

	p := c.Phase("postgres")
	p.Detail("applied Deployment")
	p.Done()
	c.Phase("graphiti").Skip("optional")
	c.Phase("spicedb").Fail()
	c.Warn("something noteworthy")
	require.NoError(t, c.Close())

	out := buf.String()
	assert.False(t, hasSGR(out), "a colorless theme must leave no color in the output:\n%q", out)
	assert.Contains(t, out, "✓", "the done marker is still drawn")
	assert.Contains(t, out, "⊘", "the skipped marker is still drawn")
	assert.Contains(t, out, "✗", "the failed marker is still drawn")
	assert.Contains(t, out, "\033[K", "the live region is still cleared and rewritten")
}

// TestChecklist_ColoredTheme_GivesEachRowStatusItsOwnColor pins that the status
// markers come from the shared palette and stay tellable apart.
//
// Distinctness is asserted rather than a literal color index, so the palette
// stays free to move: the claim is that a failed row cannot be mistaken for a
// skipped or a done one, not that failure is specifically red.
func TestChecklist_ColoredTheme_GivesEachRowStatusItsOwnColor(t *testing.T) {
	c := newChecklistWithTheme(&bytes.Buffer{}, strings.NewReader(""), false, colorTheme())

	byStatus := map[string]string{}
	for _, tc := range []struct {
		name   string
		status rowStatus
	}{
		{name: "pending", status: statusPending},
		{name: "done", status: statusDone},
		{name: "failed", status: statusFailed},
		{name: "skipped", status: statusSkipped},
	} {
		t.Run(tc.name+": marker is colored", func(t *testing.T) {
			out := c.renderRow(&row{name: "postgres", status: tc.status})
			require.True(t, hasSGR(out), "a colored theme must color the %s marker:\n%q", tc.name, out)
			byStatus[tc.name] = sgrRE.FindString(out)
		})
	}

	distinct := map[string]string{}
	for name, sgr := range byStatus {
		if other, clash := distinct[sgr]; clash {
			assert.Failf(t, "status colors collide",
				"%s and %s both render as %q; an operator cannot tell them apart", name, other, sgr)
		}
		distinct[sgr] = name
	}
}

// TestChecklist_Redraw_RewritesExactlyOneLinePerRow is the anti-scrollback-spam
// guard. This renderer redraws on every spinner tick, so its frame must stay
// exactly as tall as it was: one line per row, reached by a cursor-up of the
// same count. A theme that added a border, a title or a blank separator would
// grow the region past the number of lines redraw walks back over, and every
// tick would leave the previous frame behind instead of overwriting it.
func TestChecklist_Redraw_RewritesExactlyOneLinePerRow(t *testing.T) {
	var buf bytes.Buffer
	c := newChecklistWithTheme(&buf, strings.NewReader(""), false, colorTheme())
	c.Phase("postgres")
	c.Phase("spicedb")
	require.Equal(t, 2, c.linesDrawn, "precondition: two rows occupy two lines")

	buf.Reset()
	c.mu.Lock()
	c.redraw()
	c.mu.Unlock()

	frame := buf.String()
	assert.Equal(t, 2, strings.Count(frame, "\n"), "a redraw must emit one line per row, no more:\n%q", frame)
	assert.Equal(t, 2, strings.Count(frame, "\r\033[K"), "each row's line must be cleared before it is rewritten")
	assert.True(t, strings.HasPrefix(frame, "\033[2A"),
		"a redraw must walk back exactly as many lines as it drew:\n%q", frame)
	for _, border := range []string{"╭", "╮", "╰", "╯", "│", "─"} {
		assert.NotContains(t, frame, border, "the checklist must not grow a frame; it redraws in place")
	}
}
