//go:build darwin && arm64

package desktopcmd

import (
	"github.com/spf13/cobra"
	webview "github.com/webview/webview_go"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewWindowCmd registers the hidden `oap desktop-window` subcommand: a
// native WKWebView window pointed at a local setupui.Server URL (see
// cmd/oap/internal/desktop/setupui). It is NOT meant to be run directly by a
// user — `oap desktop`'s menubar process (desktop_darwin.go's
// spawnSetupWindow) re-execs this SAME `oap` binary with this subcommand as
// a SEPARATE CHILD PROCESS, because systray (the menubar) and webview (this
// window) both need to own their process's main thread and cannot share
// one. Two processes, two main threads; the page in this window talks back
// to the menubar process purely over HTTP against --url.
//
// This file carries the darwin/arm64 build tag: webview_go is cgo and links
// WebKit on darwin (see its #cgo directives), so keeping the real
// implementation isolated here — mirroring desktop_darwin.go /
// desktop_stub.go's split — is what lets `CGO_ENABLED=0 GOOS=linux go build
// ./cmd/oap` stay clean; desktop_window_stub.go registers the identical
// command on every other platform with a Run that fails closed instead.
func NewWindowCmd(_ *apcmd.Globals) *cobra.Command {
	var url, title string
	cmd := &cobra.Command{
		Use:    "desktop-window",
		Short:  "Internal: run the oap desktop setup window (native webview)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDesktopWindow(url, title)
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "URL of the local setup UI server to display (required)")
	cmd.Flags().StringVar(&title, "title", "OAP Desktop", "Window title")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

// runDesktopWindow opens a single native window navigated to url and blocks
// on THIS process's main thread for the window's lifetime (webview.Run()) —
// safe here because `oap desktop-window` always runs as its own dedicated
// child process, never sharing a process with systray. Returns once the
// user closes the window (or the process is killed by the parent's
// stopSetupUI on menubar quit/exit).
func runDesktopWindow(url, title string) error {
	w := webview.New(false)
	defer w.Destroy()

	// webview.New has already created NSApp and the underlying NSWindow by
	// the time it returns (its cocoa_wkwebview_engine constructor pumps a
	// temporary run loop to receive applicationDidFinishLaunching before
	// coming back — see webview.h), so both calls are safe here and MUST
	// run before w.Run() hands the main thread to the real event loop.
	//
	//   - activateDesktopWindow gives this process a Dock icon and brings
	//     the window to front/focus — see its doc for why webview_go's own
	//     built-in activation logic doesn't cover the exec'd-from-an-
	//     LSUIElement-parent case this window is always opened from.
	//   - installEditMenu is what makes Cmd-V (and Cmd-C/Cmd-X/Cmd-Z/...)
	//     work in the config form's text field — see its doc.
	activateDesktopWindow(w.Window())
	installEditMenu()

	w.SetTitle(title)
	// Sensible default size for the config form + timeline page; HintNone
	// means this is just the starting size — the window stays resizable.
	w.SetSize(760, 640, webview.HintNone)
	w.Navigate(url)
	w.Run()
	return nil
}
