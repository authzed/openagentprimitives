//go:build darwin && arm64

package desktopcmd

/*
#cgo LDFLAGS: -framework Cocoa

void apInstallURLHandler(void);
*/
import "C"

import (
	"fmt"
	"os"
)

// urlEvents carries oap:// links from the AppKit Apple Event handler to the
// Go side. Buffered so the main thread never waits on a consumer; a burst
// beyond the buffer is dropped (the install-link gate would refuse a second
// in-flight link anyway).
var urlEvents = make(chan string, 4)

// installURLEventHandler registers the kInternetEventClass/kAEGetURL handler
// that receives oap:// links — both a cold launch (the app was started BY
// the link; the event arrives once the run loop starts) and a warm one.
// fyne.io/systray owns the NSApplication delegate, so
// application:openURLs: is unavailable; an NSAppleEventManager handler is
// the delegate-free route. See apInstallURLHandler for why it registers
// twice.
//
// Must be called on the main (OS-locked) thread before systray.Run starts
// the run loop, or the will-finish-launching re-registration is missed.
func installURLEventHandler() { C.apInstallURLHandler() }

// deliverURLEvent hands one link to urlEvents without blocking, reporting
// false when the buffer is full and the link was dropped.
func deliverURLEvent(s string) bool {
	select {
	case urlEvents <- s:
		return true
	default:
		return false
	}
}

// goOAPURLEvent is called from the AppKit main thread by the Objective-C
// handler in urlevents_darwin.m. It must not block. A drop goes to stderr:
// the desktop process's logger is not reachable from a cgo callback.
//
//export goOAPURLEvent
func goOAPURLEvent(cs *C.char) {
	s := C.GoString(cs)
	if !deliverURLEvent(s) {
		fmt.Fprintf(os.Stderr, "oap desktop: install link dropped, %d links already queued (len=%d)\n", cap(urlEvents), len(s))
	}
}
