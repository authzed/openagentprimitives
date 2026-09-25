//go:build darwin && arm64

package desktopcmd

/*
#cgo LDFLAGS: -framework Cocoa

void apActivateProcess(int pid);
*/
import "C"

// activateProcess re-fronts the app owning pid — used by openSettings to
// bring an already-running settings window back to the foreground instead
// of spawning a second one. pid is the settings-window child process's own
// pid (each `oap desktop-window` re-exec is its own NSApplication, not a
// window within this menubar process), so activation goes through
// NSRunningApplication rather than the in-process NSWindow handle
// window_appkit_darwin.go uses for the setup window (which shares this
// process before its own child exists).
//
// A pid with no running app is a no-op — the caller's liveness check
// (c.Process != nil && c.ProcessState == nil) races with the child actually
// exiting, and losing that race must not crash; see apActivateProcess's nil
// guard.
func activateProcess(pid int) { C.apActivateProcess(C.int(pid)) }
