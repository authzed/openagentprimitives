// Package browser opens a URL in the host's default browser — a single
// small, dependency-free utility with no better home. See pkg/x/README.md.
//
// It is the runtime.GOOS dispatcher underneath every "open this in your
// browser" step in the repo: the mcp/oauth flow, the identity setup builtins,
// the open_url agent tool, the cluster-login callback, and the channel
// wizards' handoff. Callers call Open directly; a package that wraps it in a
// package-level var of its own is re-creating the seam this package already
// owns, and the wrapper is what gets forgotten.
//
// # Opening is suppressed inside a test binary
//
// Open refuses to reach the operating system when testing.Testing() reports a
// test binary, returning ErrSuppressed instead. That is deliberately the
// DEFAULT rather than something a test opts into: an opt-in seam is only as
// safe as every test remembering to set it, and twice in this repo's history a
// newly-written setup flow opened real browser windows on a maintainer's
// desktop during `go test` because one test did not. Forgetting now costs a
// returned error, not a window.
//
// Importing testing from non-test code is safe: since Go 1.13 the testing
// package registers its flags in testing.Init, called by the generated test
// main, so linking it into a production binary adds no flags and changes no
// behavior.
//
// A test that wants to ASSERT the URL — or to DRIVE the flow by calling back
// into it, as the OAuth and handoff flows need — installs a recorder from
// pkg/x/browser/browsertest rather than hand-rolling a package var.
package browser

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"testing"
)

// ErrSuppressed reports that Open declined to launch anything because it was
// called from a test binary with no opener installed.
//
// Returning an error rather than nil is the point. Reporting success for an
// open that never happened would let a flow wait on a consent callback nobody
// could ever complete, and would hide the very tests this suppression exists
// to protect.
//
// It is the ONLY sentinel this package defines, and the only outcome a caller
// can classify. Everything else Open returns is an opaque wrapped error: a
// launcher that is missing and one that failed to spawn are the same string
// with no way to tell them apart, and neither is worth a sentinel, because
// every call site answers all of them the same way — print the URL and carry
// on.
//
// A nil return is NOT proof a browser appeared. openOS uses cmd.Start, not
// cmd.Run, so it reports whether the launcher process was spawned and never
// waits for what it did; the commonest headless failure — xdg-open starting
// and then exiting because DISPLAY is unset — is a nil error here. Waiting
// would be worse: cmd.Run on a launcher that execs the browser itself blocks
// for as long as the user leaves the window open. This is why "print the URL
// regardless of the outcome" is the contract at every call site, rather than
// something only the error path does.
var ErrSuppressed = errors.New("browser open suppressed inside a test binary")

var (
	mu     sync.RWMutex
	opener func(string) error
)

// Open opens url in the system default browser, detected via runtime.GOOS.
//
// Errors are non-fatal for most callers — every call site prints or shows the
// URL regardless of the outcome, which is what rescues a headless host, an SSH
// session, a CI runner, and a browser that opened on the wrong machine alike.
// Do not treat a failure here as a reason to abandon the flow.
func Open(url string) error {
	if testing.Testing() {
		mu.RLock()
		fn := opener
		mu.RUnlock()
		if fn != nil {
			return fn(url)
		}
		return fmt.Errorf("%w (would have opened %s)", ErrSuppressed, url)
	}
	return launch(url)
}

// launch is the real OS dispatch, indirected through a var only so this
// package's own test can install a spy in its place and assert the spy never
// fires. That is the one claim no assertion on Open's return value can make:
// by the time a test could observe a nil error, the window it was meant to
// prevent is already on somebody's screen. Nothing outside this package can
// reach it.
var launch = openOS

// openOS is the real launch. Reached only outside a test binary.
func openOS(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default: // linux, freebsd, etc.
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser (%s): %w", runtime.GOOS, err)
	}
	return nil
}

// SetOpenerForTest makes Open call fn, and returns a function restoring the
// previous opener. Prefer pkg/x/browser/browsertest, which wires the restore
// into t.Cleanup for you; this is the primitive underneath it.
//
// It panics outside a test binary. That is what keeps the seam from becoming a
// way for production code to redirect every browser open in the process: the
// suppression above is only a guarantee if nothing can install an opener that
// outlives a test, and a panic is checkable where a doc comment is not.
func SetOpenerForTest(fn func(string) error) (restore func()) {
	if !testing.Testing() {
		panic("browser.SetOpenerForTest called outside a test binary")
	}
	mu.Lock()
	prev := opener
	opener = fn
	mu.Unlock()
	return func() {
		mu.Lock()
		opener = prev
		mu.Unlock()
	}
}
