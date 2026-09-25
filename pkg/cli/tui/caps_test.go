package tui

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapsMode(t *testing.T) {
	cases := []struct {
		name string
		caps Caps
		want mode
	}{
		{name: "TTY with color: modeTTY", caps: Caps{TTY: true, Color: true}, want: modeTTY},
		{name: "TTY without color: modePlain (color-off implies plain rendering)", caps: Caps{TTY: true, Color: false}, want: modePlain},
		{name: "no TTY: modePlain", caps: Caps{TTY: false, Color: false}, want: modePlain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.caps.mode())
		})
	}
}

func TestDetectHonorsNoColorFlagAndEnv(t *testing.T) {
	// A pipe is never a TTY, so Detect must report TTY=false/Color=false
	// regardless of the flag — this is the CI / `oap ... | tee` shape.
	r, w, err := os.Pipe()
	require.NoError(t, err, "create pipe")
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	got := Detect(w, false)
	assert.False(t, got.TTY, "a pipe is not a TTY")
	assert.False(t, got.Color, "color must be off when not a TTY")
	assert.Equal(t, modePlain, got.mode())

	t.Setenv("NO_COLOR", "1")
	assert.False(t, Detect(w, false).Color, "NO_COLOR forces color off")
}

func TestDetectWidthFallsBackWhenUnmeasurable(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err, "create pipe")
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	assert.Equal(t, defaultWidth, Detect(w, false).Width,
		"unmeasurable terminal falls back to defaultWidth, never 0")
}
