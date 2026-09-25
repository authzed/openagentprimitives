// Command claudeshim is installed as /opt/ap-toolchains/claude/bin/claude in
// the ap-toolchain-claude image, taking over the name the sandbox PATH (and
// the "claude" toolkit's declared target.binary in toolkits/claude.yaml)
// resolves. The real Claude Code binary is renamed alongside it to
// realBinaryName ("claude.real") by images/toolchain-claude/Dockerfile, so it
// is never invoked directly.
//
// Claude Code cannot see AP-staged skill bundles on its own: they land
// read-only at /skills/<LocalName>/ — the skill's own AgentSkill name, not
// its Kubernetes-object-safe MountName (see pkg/platform/podspec's
// SkillsMountPath doc and pkg/controllers/agentsession/bundles.go's
// BuildBundleSession) — but Claude Code only discovers user-level skills at
// $HOME/.claude/skills/. This shim bridges the two
// (pkg/tools/toolchain/claude.LinkStagedSkills) on every invocation, then
// hands off to the real binary via syscall.Exec (execve(2)), which REPLACES
// this process's image rather than spawning a child.
//
// That replacement is deliberate, not an optimization. A fork+wait hand-off
// (os/exec.Command) creates a shim-parent / claude.real-child process tree,
// and this package installs no signal.Notify and forwards nothing — so
// killing the shim orphans a still-running, still-token-burning claude.real
// that goes on holding the sandbox open. Signal-forwarding code would not
// close that hole either: SIGKILL cannot be caught or forwarded by anyone,
// and context-cancellation-based termination — the normal way a sandboxed
// tool call gets stopped — ends in exactly that. Only removing the second
// process closes it. With syscall.Exec there is one process: its PID never
// changes, so anything tracking that PID keeps tracking the right thing, and
// argv/stdio/the exit code become OS guarantees of execve(2) — not
// passthrough code (a forgotten cmd.Stdout, a dropped exit code) that can
// regress.
//
// syscall.Exec is Unix-only (linux/darwin/dragonfly/freebsd/netbsd/openbsd/
// solaris/aix — see $GOROOT/src/syscall/exec_unix.go's build constraint).
// That is fine: this binary only ever runs inside a Linux sandbox container.
// darwin is in that same constraint, so this also builds and runs unchanged
// on the macOS machine this repo is developed on — verified with
// `go build ./...` and `go test ./internal/cmd/claudeshim/...` on darwin; no
// build tag is needed on either platform.
//
// A failed skill bridge does NOT abort the hand-off: reviewing without the
// staged methodology is a degraded outcome, not a broken tool call. But it
// must be loud — this is the fix for a silent quality failure (skills
// mounted and never found, an agent improvising a review while still posting
// an artifact), so a failed bridge is reported to stderr, greppable, before
// the hand-off, rather than moving the same silence one layer down.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/claude"
)

const (
	// skillsMountPath mirrors pkg/platform/podspec's exported SkillsMountPath
	// (see its doc comment there for why this shim hand-duplicates rather than
	// imports it): every sandbox container that opts into skill bundles gets
	// them staged read-only at this path, regardless of which toolchains are
	// present.
	skillsMountPath = "/skills"
	// realBinaryName is what images/toolchain-claude/Dockerfile renames the
	// actual Claude Code binary to, alongside this shim, so the shim can claim
	// the name "claude" in $PATH without colliding with it. The "." makes it
	// visually distinct from a real subcommand or a versioned name, and keeps
	// it out of the way of `claude <anything>` completion/typo confusion.
	realBinaryName = "claude.real"
	// execFailureExitCode is returned when this shim cannot even START the
	// hand-off (the real binary is missing, not executable, ...) — a broken
	// image build, not a bridge failure. Chosen to match the shell convention
	// for "command not found". Once the hand-off actually starts, this shim's
	// own exit code no longer exists to return: syscall.Exec has replaced the
	// process, and the real binary's exit code becomes the whole process's.
	execFailureExitCode = 127
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claudeshim: resolve own executable path: %v\n", err)
		os.Exit(126)
	}
	// run returns ONLY on failure: syscall.Exec replaces this process's image
	// on success, so there is no "after" for a success case to fall through
	// to — os.Exit below is reached only when the hand-off never started.
	if err := run(exe, skillsMountPath, os.Args, os.Environ(), os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "claudeshim: exec %s: %v\n", realBinaryPath(exe), err)
		os.Exit(execFailureExitCode)
	}
}

// run performs the bridge-then-hand-off sequence. It returns ONLY when the
// hand-off itself could not be started (the real binary is missing, not
// executable, ...): on success, syscall.Exec has already replaced the
// process image, so this function — and everything after its call site in
// main — never runs again.
func run(shimPath, skillsDir string, argv, environ []string, stderr io.Writer) error {
	bridgeStagedSkills(skillsDir, environ, stderr)

	real := realBinaryPath(shimPath)
	// argv[0] becomes the resolved real path, mirroring how a shell `exec
	// real "$@"` reproduces argv; argv[1:] is this shim's own argv minus its
	// own name.
	execArgv := append([]string{real}, argv[1:]...)
	return syscall.Exec(real, execArgv, environ)
}

// bridgeStagedSkills links AP-staged skills into Claude Code's discovery
// path. Any failure — including HOME being unset in the environment this
// shim was launched with — is written to stderr and otherwise swallowed: the
// caller must still hand off to claude.real, degraded but running, never
// silently and never aborted.
func bridgeStagedSkills(skillsDir string, environ []string, stderr io.Writer) {
	home, ok := homeFromEnviron(environ)
	if !ok {
		fmt.Fprintf(stderr, "claudeshim: HOME not set; skipping staged-skill bridge (%s), continuing without them\n", skillsDir)
		return
	}
	if err := claude.LinkStagedSkills(skillsDir, home); err != nil {
		fmt.Fprintf(stderr, "claudeshim: bridging staged skills from %s into %s/.claude/skills failed, continuing without them: %v\n",
			skillsDir, home, err)
	}
}

// realBinaryPath resolves the real Claude Code binary's path from where this
// shim itself is running: same directory, realBinaryName. Relative to the
// shim rather than a baked-in absolute constant so the path resolution is a
// pure function testable without the toolchain's real mount layout — though
// in production the two coincide, since the toolchain payload is
// relocation-free (build path == mount path; see
// v1alpha1.ToolchainRootPath's doc).
func realBinaryPath(shimPath string) string {
	return filepath.Join(filepath.Dir(shimPath), realBinaryName)
}

// homeFromEnviron reads HOME out of an os.Environ()-shaped slice, returning
// the first match (mirroring libc getenv semantics) so bridgeStagedSkills
// derives HOME from exactly the environment the real binary will also see
// (syscall.Exec is handed this same slice), rather than a second, possibly
// inconsistent lookup.
func homeFromEnviron(environ []string) (string, bool) {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			return v, true
		}
	}
	return "", false
}
