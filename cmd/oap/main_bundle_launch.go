// cmd/oap/main_bundle_launch.go
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// desktopBundleMarker is the path fragment that only ever appears when this
// binary is running from inside a macOS .app bundle assembled by `mage
// desktop:app` (magefiles/desktop.go's Desktop.App writes build/desktop/out/
// oap.app/Contents/MacOS/oap — the SAME layout desktopResourcesDir's doc
// comment in cmd/oap/internal/desktopcmd/run_darwin.go documents:
// "Contents/MacOS/ap -> ../Resources").
const desktopBundleMarker = ".app/Contents/MacOS/"

// maybeInjectDesktopSubcommand makes a double-clicked oap.app behave like
// `oap desktop` without a second launcher binary or an Info.plist
// CFBundleArguments hack (LaunchServices ignores CFBundleArguments for a
// user double-click anyway — that key only ever affects programmatic opens).
// CFBundleExecutable in build/desktop/Info.plist is this SAME `oap` binary,
// so a Finder double-click execs it with ZERO arguments — which cobra's root
// command would otherwise resolve to printing help text into nowhere (no
// Terminal is attached to a Finder launch), i.e. a silent, useless launch.
//
// The fix: detect that specific shape and inject the "desktop" subcommand
// before cobra ever parses argv. The check is intentionally narrow so it can
// never surprise a deliberate CLI invocation — see injectDesktopSubcommand's
// doc for exactly what "narrow" means.
//
// Restricted to darwin (the only OS `mage desktop:app` ever targets — see
// that target's runtime.GOOS guard). `oap desktop` itself is further
// platform-gated to darwin/arm64 (run_darwin.go vs the "unsupported platform"
// fallback used on every other GOOS/GOARCH — see
// cmd/oap/internal/desktopcmd/stub.go), so even a hypothetical darwin/amd64
// build hitting this path fails loud rather than silently, via that stub.
func maybeInjectDesktopSubcommand(args []string) []string {
	if runtime.GOOS != "darwin" {
		return args
	}
	exe, err := os.Executable()
	if err != nil {
		// Can't resolve our own path — leave argv untouched; NewRootCmd's
		// normal help/usage output is a safe fallback, not a silent failure
		// (the user still sees SOMETHING if this is ever run from a
		// Terminal, which is the only way os.Executable() plausibly fails).
		return args
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return injectDesktopSubcommand(args, exe)
}

// injectDesktopSubcommand is the pure, directly-testable core of
// maybeInjectDesktopSubcommand — split out so tests can supply a synthetic
// exePath without needing an actual .app bundle on disk.
//
// Fires ONLY when BOTH hold:
//   - len(args) == 1 — the bare program name, no flags or subcommand at all.
//     `oap --help`, `oap install`, or a no-op `oap` a developer explicitly typed
//     at a prompt with trailing args are all left untouched.
//   - the resolved executable path contains desktopBundleMarker — the one
//     place `mage desktop:app` ever installs this binary. A `go build -o
//     bin/oap ./cmd/oap` output, or a Homebrew-installed `oap`, never matches
//     this, so ordinary CLI usage everywhere else is unaffected.
func injectDesktopSubcommand(args []string, exePath string) []string {
	if len(args) != 1 {
		return args
	}
	if !strings.Contains(filepath.ToSlash(exePath), desktopBundleMarker) {
		return args
	}
	return append(args, "desktop")
}
