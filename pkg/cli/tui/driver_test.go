package tui

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriverForResolvesOneDriverPerCapability(t *testing.T) {
	cases := []struct {
		name   string
		params DriverParams
		want   Driver
	}{
		{
			name:   "TTY with color: the alt-screen driver",
			params: DriverParams{Theme: NewTheme(Caps{TTY: true, Color: true, Width: 80})},
			want:   &ttyDriver{},
		},
		{
			name:   "TTY without color: the plain driver, since color-off is not a request for a monochrome takeover",
			params: DriverParams{Theme: NewTheme(Caps{TTY: true, Color: false, Width: 80})},
			want:   &plainDriver{},
		},
		{
			name:   "no TTY: the plain driver",
			params: DriverParams{Theme: NewTheme(Caps{TTY: false, Color: false, Width: 80})},
			want:   &plainDriver{},
		},
		{
			name: "non-interactive on a full TTY: the fail-closed driver still wins",
			params: DriverParams{
				Theme:          NewTheme(Caps{TTY: true, Color: true, Width: 80}),
				NonInteractive: true,
			},
			want: nonInteractiveDriver{},
		},
		{
			name: "full TTY but In is a script: the plain driver, which is the one that reads In",
			params: DriverParams{
				Theme: NewTheme(Caps{TTY: true, Color: true, Width: 80}),
				In:    strings.NewReader("an answer\n"),
			},
			want: &plainDriver{},
		},
		{
			name: "full TTY and In is nil: the alt-screen driver, since nil means read os.Stdin",
			params: DriverParams{
				Theme: NewTheme(Caps{TTY: true, Color: true, Width: 80}),
				In:    nil,
			},
			want: &ttyDriver{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DriverFor(tc.params)
			require.NotNil(t, got, "DriverFor must always resolve a driver")
			assert.IsType(t, tc.want, got)
		})
	}
}

// TestATerminalWriterIsNotEvidenceAnybodyCanAnswer is the rule stated over the
// readers a run is actually handed, rather than over the one case a test cannot
// build: a terminal reader needs a pseudo-terminal.
//
// Every row here is a reader the alt-screen driver would THROW AWAY — it takes
// only Out and hands bubbletea process stdin — so selecting it would read a
// stream nobody supplied answers on. huh's accessible renderer then turns the
// end of that stream into each field's default with a nil error, which is how a
// run reports answers nobody gave.
//
// /dev/null earns its own row: it is a character device, so the
// os.ModeCharDevice test this project uses elsewhere calls it interactive.
// `some-command < /dev/null` is precisely the case the rule exists for, so the
// weaker check would have passed the one input it was written to catch.
func TestATerminalWriterIsNotEvidenceAnybodyCanAnswer(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = devNull.Close() })

	// The only capabilities under which the rule does anything.
	terminal := NewTheme(Caps{TTY: true, Color: true, Width: 120})

	cases := []struct {
		name string
		in   io.Reader
		want Driver
	}{
		{name: "a scripted reader: the plain driver reads it", in: strings.NewReader("answer\n"), want: &plainDriver{}},
		{name: "a redirected /dev/null: the plain driver, despite it being a char device", in: devNull, want: &plainDriver{}},
		{name: "a pipe wrapper that is not a file at all: the plain driver", in: io.LimitReader(devNull, 1), want: &plainDriver{}},
		{name: "no reader at all: the alt-screen driver, since nil means read os.Stdin", in: nil, want: &ttyDriver{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.IsType(t, tc.want, DriverFor(DriverParams{Theme: terminal, In: tc.in}))
		})
	}
}

// TestReaderCanAnswerKeepsNilMeaningReadStdin pins the trap directly: Plain
// treats a nil reader as huh's "read os.Stdin" signal and must not wrap it, so
// nil has to stay the one non-file value that still permits the alt screen.
// Every command that leaves Options.In unset depends on it.
func TestReaderCanAnswerKeepsNilMeaningReadStdin(t *testing.T) {
	assert.True(t, readerCanAnswer(nil), "a nil reader means os.Stdin, not 'nobody is there'")
	assert.False(t, readerCanAnswer(strings.NewReader("")), "a script is not somebody typing")
}

func TestDriverForWithNoThemeYieldsAPlainUncoloredDriver(t *testing.T) {
	// Zero Caps is a legitimate terminal (a pipe), so a missing theme must
	// degrade to the driver that terminal would have gotten, not panic.
	var out bytes.Buffer
	got := DriverFor(DriverParams{In: strings.NewReader(""), Out: &out})

	require.IsType(t, &plainDriver{}, got)
	assert.NotNil(t, got.(*plainDriver).theme, "a nil theme must be substituted, not carried")
}

func TestTTYWithNoChromeStillRendersAFrame(t *testing.T) {
	d := TTY(TTYOpts{Out: io.Discard, Theme: NewTheme(Caps{TTY: true, Color: true, Width: 80})})

	require.IsType(t, &ttyDriver{}, d)
	require.NotNil(t, d.(*ttyDriver).chrome, "a nil chrome must be substituted with a railless one")
	assert.Equal(t, -1, d.(*ttyDriver).chrome.StepIndex("anything"),
		"a railless chrome resolves no step, which Render marks every step pending for")
}
