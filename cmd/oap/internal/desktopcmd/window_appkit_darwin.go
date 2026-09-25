//go:build darwin && arm64

package desktopcmd

/*
#cgo LDFLAGS: -framework Cocoa

#include <stdlib.h>

void apWindowActivate(void *nsWindowPtr);
void apInstallEditMenu(void);
*/
import "C"
import "unsafe"

// activateDesktopWindow forces the process's activation policy to
// NSApplicationActivationPolicyRegular (Dock icon, Cmd-Tab entry,
// focusable) and brings nsWindow to the front.
//
// webview_go's own cocoa_wkwebview_engine already does this same
// setActivationPolicy/activateIgnoringOtherApps dance in
// on_application_did_finish_launching (see libs/webview/include/webview.h)
// — but ONLY when its is_app_bundled() check returns false. That check is
// just "does the running executable live under a path ending in .app",
// which is true for `oap desktop-window`: it's the SAME `oap` binary
// re-exec'd out of the .app's own Contents/MacOS (see spawnSetupWindow in
// desktop_darwin.go), so is_app_bundled() reports true and webview_go
// skips the call — its comment explains the assumption: "Bundled apps
// launched from Finder are activated automatically." That assumption only
// holds when LaunchServices does the launching. Here the parent menubar
// process (itself LSUIElement, so it has no Dock icon of its own) starts
// this window via a plain exec, bypassing LaunchServices entirely, so the
// child inherits none of that automatic activation and defaults to an
// accessory/background policy — no Dock icon, not Cmd-Tabbable, not
// focusable. Calling this ourselves after webview.New(...) (once NSApp
// and the window already exist) closes that gap.
func activateDesktopWindow(nsWindow unsafe.Pointer) {
	C.apWindowActivate(nsWindow)
}

// installEditMenu installs a minimal NSApp main menu with a standard Edit
// submenu (Undo/Redo/Cut/Copy/Paste/Select All) bound to the standard
// first-responder selectors. A bare WKWebView window built without a nib
// or NSApplicationMain has no main menu at all, and AppKit only resolves
// Cmd-key equivalents (Cmd-V for paste, etc.) by walking NSApp's menu item
// tree — with no menu, there is nothing for AppKit to match those key
// events against, so the standard editing shortcuts do nothing in the
// config form's text fields even though WKWebView itself implements
// paste:/copy:/etc. Must be called after webview.New(...) so NSApp exists.
func installEditMenu() {
	C.apInstallEditMenu()
}
