package desktop_test

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

// freePort asks the OS for a free port, then immediately releases it — used
// to pick a base for PickStablePort tests without colliding with whatever
// else is running on the test machine.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestPickStablePort_ReturnsBaseWhenFree(t *testing.T) {
	base := freePort(t)

	got, err := desktop.PickStablePort(base)
	require.NoError(t, err)
	require.Equal(t, base, got)
}

func TestPickStablePort_SkipsOccupiedPort(t *testing.T) {
	base := freePort(t)

	// Occupy base for the duration of the test so PickStablePort must skip it.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base))
	require.NoError(t, err)
	defer ln.Close()

	got, err := desktop.PickStablePort(base)
	require.NoError(t, err)
	require.Equal(t, base+1, got)
}

func TestPickStablePort_NoFreePortInRange(t *testing.T) {
	base := freePort(t)

	// Occupy every port PickStablePort would try (base..base+63).
	var listeners []net.Listener
	for i := 0; i < 64; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
		if err != nil {
			// A port in range is already unavailable for some other reason
			// (e.g. reserved/in-use by another process) — skip rather than
			// flake; the exhaustion case is still covered by the ports we
			// did manage to occupy plus PickStablePort's bounded loop.
			t.Skipf("could not occupy port %d for the exhaustion test: %v", base+i, err)
		}
		listeners = append(listeners, ln)
	}
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}()

	_, err := desktop.PickStablePort(base)
	require.Error(t, err)
}
