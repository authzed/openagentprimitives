//go:build darwin && arm64

// cmd/oap/internal/desktopcmd/activate_darwin.m
//
// Small AppKit helper for re-fronting the settings window's child process
// (see activate_darwin.go, called from openSettings in run_darwin.go). Kept
// as a separate .m file (compiled by cgo automatically, no explicit
// -x objective-c CFLAGS needed) rather than folded into the .go file's cgo
// preamble, since Cocoa.h needs a real Objective-C compilation unit — same
// split as window_appkit_darwin.m.

#import <Cocoa/Cocoa.h>

// apActivateProcess brings the NSRunningApplication owning pid to the front,
// across all its windows. Unlike window_appkit_darwin.m's apWindowActivate
// (which activates THIS process and a specific NSWindow it already has a
// pointer to), the settings window is a separate `oap desktop-window` child
// process — a distinct NSApplication this process has no NSWindow handle
// for — so activation goes through NSRunningApplication by pid instead.
//
// A pid with no matching running application (the child already exited,
// racing the caller's liveness check) is a no-op: runningApplicationWithProcessIdentifier:
// returns nil, and this simply skips activation rather than crashing.
void apActivateProcess(int pid) {
  @autoreleasepool {
    NSRunningApplication *app =
        [NSRunningApplication runningApplicationWithProcessIdentifier:(pid_t)pid];
    if (app != nil) {
      [app activateWithOptions:(NSApplicationActivateAllWindows |
                                 NSApplicationActivateIgnoringOtherApps)];
    }
  }
}
