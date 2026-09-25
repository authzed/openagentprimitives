package tui

import (
	"context"
	"io"
	"reflect"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What these tests can and cannot prove.
//
// CAN: that a run's Inline reaches driver selection, that driver selection
// carries it onto the terminal driver, and that the terminal driver's program
// options then carry — or omit — tea.WithAltScreen. Those three links are the
// whole of the wiring, and each is separately breakable.
//
// CANNOT: anything about what a terminal draws. bubbletea's alternate screen is
// a sequence written to a real terminal; there is no pseudo-terminal in this
// suite by design (see driver_tty_test.go), so no assertion here observes a
// rendered frame. Nothing below is named as though it did.

// optionID names the bubbletea constructor an option came out of.
//
// A tea.ProgramOption is a func(*Program) that mutates unexported state, so an
// option cannot be inspected for what it DOES; what it can be asked is where it
// came from. Every constructor returns a closure whose runtime name ends
// "<Constructor>.funcN" — either under bubbletea's own package path, or under
// the caller's when the constructor was inlined into it, which is why the
// package part is dropped and the closure counter with it.
func optionID(o tea.ProgramOption) string {
	fn := runtime.FuncForPC(reflect.ValueOf(o).Pointer())
	if fn == nil {
		return ""
	}
	parts := strings.Split(fn.Name(), ".")
	if len(parts) < 2 {
		return fn.Name()
	}
	return parts[len(parts)-2]
}

// carriesAltScreen reports whether opts contains the option tea.WithAltScreen
// produces. The identity it compares against is taken from the library rather
// than written down here, so renaming the option upstream fails this loudly
// instead of silently matching nothing.
func carriesAltScreen(opts []tea.ProgramOption) bool {
	want := optionID(tea.WithAltScreen())
	for _, o := range opts {
		if optionID(o) == want {
			return true
		}
	}
	return false
}

// The identity check above is only worth anything if a DIFFERENT option does
// not match it. Without this, carriesAltScreen returning true for everything
// would make every assertion below vacuous in the same direction.
func TestCarriesAltScreenIdentifiesTheOptionItself(t *testing.T) {
	require.NotEmpty(t, optionID(tea.WithAltScreen()), "the option must be identifiable at all")
	assert.True(t, carriesAltScreen([]tea.ProgramOption{tea.WithAltScreen()}),
		"the alt-screen option must be recognised")
	assert.False(t, carriesAltScreen([]tea.ProgramOption{tea.WithOutput(io.Discard), tea.WithoutSignals()}),
		"or the test proves nothing: other options must not match it")
}

// TestInlineReachesTheProgramOptions walks the three links of the wiring in one
// table, so a break anywhere along it names which link broke.
func TestInlineReachesTheProgramOptions(t *testing.T) {
	terminal := NewTheme(Caps{TTY: true, Color: true, Width: 80})

	cases := []struct {
		name string
		// inline is what the RUN declares.
		inline bool
		// wantAltScreen is what the program is then built with.
		wantAltScreen bool
	}{
		{
			name:          "a run that owns the terminal: the program takes the alternate screen",
			inline:        false,
			wantAltScreen: true,
		},
		{
			name:          "a run whose answers depend on the output around it: no alternate screen",
			inline:        true,
			wantAltScreen: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Link 1: the run's Options carry it to driver selection.
			params := Options{Theme: terminal, Inline: tc.inline}.driverParams(nil)
			require.Equal(t, tc.inline, params.Inline, "the run's decision must reach driver selection")

			// Link 2: driver selection carries it onto the terminal driver.
			d := DriverFor(params)
			td, ok := d.(*ttyDriver)
			require.True(t, ok, "a terminal with a reader at it resolves the terminal driver")
			require.Equal(t, tc.inline, td.inline, "the driver must carry the run's decision")

			// Link 3: the driver builds bubbletea's options from it.
			assert.Equal(t, tc.wantAltScreen, carriesAltScreen(td.programOptions(context.Background())),
				"the alternate screen is the one thing Inline changes")
		})
	}
}

// Inline must not be a way to reach a terminal question. It says which buffer
// the TERMINAL driver draws in, and every other driver ignores it — otherwise
// a caller could set Inline and have a run prompt where DriverFor had already
// decided nobody was there to answer.
func TestInlineNeverChangesWhichDriverIsResolved(t *testing.T) {
	terminal := NewTheme(Caps{TTY: true, Color: true, Width: 80})

	cases := []struct {
		name   string
		params DriverParams
		want   Driver
	}{
		{
			name:   "inline on a piped writer: still the line-oriented driver",
			params: DriverParams{Theme: NewTheme(Caps{}), Inline: true},
			want:   &plainDriver{},
		},
		{
			name:   "inline with answers on a script: still the driver that reads them",
			params: DriverParams{Theme: terminal, In: strings.NewReader("answer\n"), Inline: true},
			want:   &plainDriver{},
		},
		{
			name:   "inline under --non-interactive: still the fail-closed driver",
			params: DriverParams{Theme: terminal, NonInteractive: true, Inline: true},
			want:   nonInteractiveDriver{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.IsType(t, tc.want, DriverFor(tc.params))
		})
	}
}

// Everything except the alternate screen is the same run. The chrome is what a
// mid-command question is most at risk of losing — the rule says a run states
// where it draws, not whether it is framed — so it is asserted rather than
// assumed.
func TestInlineChangesNothingButTheAltScreen(t *testing.T) {
	th := NewTheme(Caps{TTY: true, Color: true, Width: 80})
	ch := NewChrome("oap · demo", steps("First", "Second"), th)

	full := TTY(TTYOpts{Out: io.Discard, Theme: th, Chrome: ch}).(*ttyDriver)
	inline := TTY(TTYOpts{Out: io.Discard, Theme: th, Chrome: ch, Inline: true}).(*ttyDriver)

	assert.Same(t, full.chrome, inline.chrome, "an inline run keeps the chrome")
	assert.Same(t, full.theme, inline.theme, "an inline run keeps the theme")
	assert.Equal(t, full.chrome.BodyWidth(), inline.chrome.BodyWidth(),
		"an inline run is measured at the same width, so its notes fit the same way")

	fullOpts := full.programOptions(context.Background())
	inlineOpts := inline.programOptions(context.Background())
	assert.Len(t, inlineOpts, len(fullOpts)-1, "exactly one option separates the two")
	assert.True(t, carriesAltScreen(fullOpts))
	assert.False(t, carriesAltScreen(inlineOpts))
}
