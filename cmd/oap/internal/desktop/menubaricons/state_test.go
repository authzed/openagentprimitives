package menubaricons

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStateBasename(t *testing.T) {
	want := map[State]string{
		StateSetup: "setup", StateRunning: "running", StateShuttingDown: "shuttingdown",
		StateStopped: "stopped", StateUninstalling: "uninstalling", StateError: "error",
		StateQuitting: "quitting",
	}
	for s, w := range want {
		require.Equal(t, w, s.Basename())
	}
}

func TestStateAnimatedAndFrameCount(t *testing.T) {
	for _, s := range []State{StateSetup, StateShuttingDown, StateUninstalling, StateQuitting} {
		require.True(t, s.Animated(), "%s should animate", s.Basename())
		require.Equal(t, 12, s.FrameCount())
	}
	for _, s := range []State{StateRunning, StateStopped, StateError} {
		require.False(t, s.Animated(), "%s should be static", s.Basename())
		require.Equal(t, 1, s.FrameCount())
	}
}

func TestAllStatesExhaustive(t *testing.T) {
	require.Len(t, AllStates(), 7)
}
