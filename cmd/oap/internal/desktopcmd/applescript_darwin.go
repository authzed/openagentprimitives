//go:build darwin && arm64

package desktopcmd

import "os/exec"

// osascript builds an osascript command that runs body inside a run handler
// with args bound to argv, so body reads its text as (item N of argv). Text
// is never spliced into the script source: Go's %q is not AppleScript string
// syntax (it emits \uXXXX and \x1b escapes AppleScript rejects as a syntax
// error), so a dialog quoting arbitrary text — a bundle's description, a
// registry error — could otherwise fail to appear at all. The "--" stops
// osascript reading an argument that starts with "-" as one of its options.
func osascript(body string, args ...string) *exec.Cmd {
	return exec.Command("osascript", append([]string{"-e", "on run argv", "-e", body, "-e", "end run", "--"}, args...)...)
}
