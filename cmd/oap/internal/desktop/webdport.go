package desktop

import (
	"fmt"
	"net"
)

// webdPortScanRange bounds how many candidate ports PickStablePort tries
// before giving up. base..base+webdPortScanRange-1.
const webdPortScanRange = 64

// PickStablePort finds a local TCP port free to bind on 127.0.0.1, starting
// at base and incrementing on collision (base, base+1, …), bounded by
// webdPortScanRange tries.
//
// The port is "stable" in the sense that base is a fixed constant
// (desktopWebdPortBase in cmd/oap/internal/desktopcmd/run_darwin.go) — an
// ordinary relaunch with nothing else bound to that port picks the SAME port
// every time, rather than a random one. That matters here because the port
// doubles as part of webd's externally-visible base URL (see the
// spicebox-webd-external-url ConfigMap): the desktop app sets that URL once
// at bring-up and the port-forward must land on the exact same port, or
// webd rejects every request as a base-URL mismatch (404).
//
// Returns a clear error (never a silent fallback) if no free port is found
// in range — the caller must not proceed as though a port was chosen.
func PickStablePort(base int) (int, error) {
	for i := 0; i < webdPortScanRange; i++ {
		port := base + i
		if isPortFree(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("desktop: no free port found in range %d-%d", base, base+webdPortScanRange-1)
}

// isPortFree reports whether port can be bound on 127.0.0.1 right now, by
// actually binding it and immediately closing the listener. This is a
// point-in-time check only — see PickStablePort's caller for how a
// TOCTOU loss (something else grabs the port between this check and the
// real port-forward bind) is surfaced rather than silently misrouted.
func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
