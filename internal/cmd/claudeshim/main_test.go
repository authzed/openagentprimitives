package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRealBinaryPath(t *testing.T) {
	cases := []struct {
		name     string
		shimPath string
		want     string
	}{
		{
			name:     "toolchain layout",
			shimPath: "/opt/ap-toolchains/claude/bin/claude",
			want:     "/opt/ap-toolchains/claude/bin/claude.real",
		},
		{
			name:     "resolves relative to whatever directory the shim actually runs from",
			shimPath: filepath.Join("some", "other", "dir", "claude"),
			want:     filepath.Join("some", "other", "dir", "claude.real"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, realBinaryPath(tc.shimPath))
		})
	}
}

func TestHomeFromEnviron(t *testing.T) {
	cases := []struct {
		name     string
		environ  []string
		wantHome string
		wantOK   bool
	}{
		{name: "present among other vars", environ: []string{"PATH=/bin", "HOME=/tmp", "FOO=bar"}, wantHome: "/tmp", wantOK: true},
		{name: "absent", environ: []string{"PATH=/bin", "FOO=bar"}, wantHome: "", wantOK: false},
		{name: "empty environ", environ: nil, wantHome: "", wantOK: false},
		{name: "first match wins on duplicate (mirrors libc getenv)", environ: []string{"HOME=/first", "HOME=/second"}, wantHome: "/first", wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := homeFromEnviron(tc.environ)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantHome, got)
		})
	}
}

// TestRun_ReturnsErrorWhenRealBinaryCannotBeExecuted is safe to run directly,
// in this test's own process: syscall.Exec only replaces the process image
// on SUCCESS. Against a real binary that does not exist, execve(2) fails
// with ENOENT and returns to the caller, so run() returning here (rather
// than the process silently becoming something else) is exactly the
// behavior under test.
func TestRun_ReturnsErrorWhenRealBinaryCannotBeExecuted(t *testing.T) {
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "claude") // no claude.real written next to it
	skillsDir := filepath.Join(t.TempDir(), "no-skills-mount")
	environ := []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}

	var stderr bytes.Buffer
	err := run(shimPath, skillsDir, []string{"claude"}, environ, &stderr)
	require.Error(t, err, "syscall.Exec against a nonexistent real binary must return an error, not silently continue")
}

// writeStubRealBinary installs a fake claude.real: a shell script (mirrors
// pkg/channels/channelkinds/slack/appprovision's writeStubSlack pattern for
// exec-based tests) that PROVES what it actually received by writing its own
// argv, stdin, and one named env var to markerPath, printing a distinctive
// line to stdout, and exiting with a distinctive, non-zero code — so an
// assertion on it cannot be satisfied by anything claudeshim computed itself,
// only by the stub having actually run under syscall.Exec.
func writeStubRealBinary(t *testing.T, dir, markerPath string, exitCode int) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\n{\n  printf 'argv:%%s\\n' \"$*\"\n  printf 'stdin:'\n  cat\n  printf 'env:%%s\\n' \"$CLAUDESHIM_MARKER_ENV\"\n} > %q\necho stub-stdout-marker\nexit %d\n",
		markerPath, exitCode)
	require.NoError(t, os.WriteFile(filepath.Join(dir, realBinaryName), []byte(script), 0o755))
}

// runViaHelperProcess spawns a FRESH OS process that re-invokes this test
// binary as TestHelperProcess (the standard library's GO_WANT_HELPER_PROCESS
// pattern — see os/exec_test.go), which calls run() for real and lets
// syscall.Exec replace THAT process's image — never this test's own. The
// return values are everything observable about the whole process from the
// outside: once the hand-off succeeds there is no "the shim" and "the real
// binary" left to distinguish, only one process and its exit code/output,
// which is exactly the property under test.
//
// env is the COMPLETE environment for the spawned process beyond a minimal
// PATH and the test-harness plumbing vars — deliberately not layered on top
// of this test binary's own os.Environ(), so a case testing "HOME absent"
// cannot be quietly satisfied by HOME the outer test process happens to have.
func runViaHelperProcess(t *testing.T, shimPath, skillsDir string, env, args []string, stdin string) (exitCode int, stdout, stderr string) {
	t.Helper()
	cmdArgs := append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append([]string{
		"GO_WANT_HELPER_PROCESS=1",
		"CLAUDESHIM_TEST_SHIM_PATH=" + shimPath,
		"CLAUDESHIM_TEST_SKILLS_DIR=" + skillsDir,
		"PATH=/usr/bin:/bin",
	}, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err == nil {
		return 0, outBuf.String(), errBuf.String()
	}
	var exitErr *exec.ExitError
	require.ErrorAsf(t, err, &exitErr,
		"helper process must exit with a normal (possibly non-zero) exit code, not fail to start: %v (stderr=%s)", err, errBuf.String())
	return exitErr.ExitCode(), outBuf.String(), errBuf.String()
}

// TestHelperProcess is not a real test: under a normal `go test` run
// GO_WANT_HELPER_PROCESS is unset, so it returns immediately and contributes
// nothing. runViaHelperProcess re-invokes the compiled test binary with that
// env var set and -test.run scoped to just this function, turning THIS
// function into the actual claudeshim under test for that one subprocess —
// including really calling syscall.Exec, which is exactly what a helper
// process (rather than an in-process call) makes safe to do.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	shimPath := os.Getenv("CLAUDESHIM_TEST_SHIM_PATH")
	skillsDir := os.Getenv("CLAUDESHIM_TEST_SKILLS_DIR")
	argv := append([]string{"claude"}, helperArgs(os.Args)...)
	if err := run(shimPath, skillsDir, argv, os.Environ(), os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "claudeshim-test-helper: hand-off never started: %v\n", err)
		os.Exit(execFailureExitCode)
	}
	// Unreachable when the hand-off actually starts: syscall.Exec has already
	// replaced this process.
}

// helperArgs strips everything up to and including the "--" separator that
// runViaHelperProcess's `-test.run=... --` re-invocation inserts, leaving
// just the args meant for run()'s argv.
func helperArgs(all []string) []string {
	for i, a := range all {
		if a == "--" {
			return all[i+1:]
		}
	}
	return nil
}

func TestRun_HandsOffToRealBinary_PassesThroughArgvEnvAndStdin(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	writeStubRealBinary(t, dir, marker, 42)

	shimPath := filepath.Join(dir, "claude")
	// No /skills mount in this scenario: the bridge must no-op silently
	// (verified separately below) so stderr here stays clean.
	skillsDir := filepath.Join(t.TempDir(), "no-skills-mount")
	env := []string{"HOME=" + t.TempDir(), "CLAUDESHIM_MARKER_ENV=proves-env-passthrough"}

	code, stdout, stderr := runViaHelperProcess(t, shimPath, skillsDir, env,
		[]string{"--print", "hello world"}, "hello-from-outer-test-stdin")

	assert.Equal(t, 42, code,
		"the OS process's own exit code must be the real binary's — after syscall.Exec there is no shim process left to report a different one")
	assert.Contains(t, stdout, "stub-stdout-marker", "stdout must reach the caller (an OS guarantee of exec, not shim code)")
	assert.Empty(t, stderr, "no bridge failure occurred (absent /skills is not an error), stderr must stay quiet")

	body, err := os.ReadFile(marker)
	require.NoError(t, err, "the marker only exists if the real binary actually ran")
	assert.Contains(t, string(body), "argv:--print hello world", "argv (minus argv[0]) must pass through verbatim, in order")
	assert.Contains(t, string(body), "stdin:hello-from-outer-test-stdin", "stdin must reach the real binary")
	assert.Contains(t, string(body), "env:proves-env-passthrough", "env must reach the real binary via the exact environ handed to syscall.Exec")
}

func TestRun_DegradesLoudlyOnBridgeFailureButStillHandsOff(t *testing.T) {
	cases := []struct {
		name          string
		env           func(t *testing.T) []string
		skillsDir     func(t *testing.T) string
		wantStderrHas []string
	}{
		{
			name: "HOME not set in the environment",
			env: func(t *testing.T) []string {
				return nil // deliberately no HOME=; runViaHelperProcess does not inherit the outer test's own HOME
			},
			skillsDir: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "skills") // never reached: HOME is checked first
			},
			wantStderrHas: []string{"claudeshim", "HOME"},
		},
		{
			name: "staged skills present but $HOME/.claude already exists as a file",
			env: func(t *testing.T) []string {
				home := t.TempDir()
				// Forces LinkStagedSkills's os.MkdirAll(home/.claude/skills) to fail
				// deterministically and portably, without permission tricks: MkdirAll
				// through a path component that is a regular file always errors.
				require.NoError(t, os.WriteFile(filepath.Join(home, ".claude"), []byte("not a directory"), 0o644))
				return []string{"HOME=" + home}
			},
			skillsDir: func(t *testing.T) string {
				skills := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(skills, "code-review"), 0o755))
				return skills
			},
			wantStderrHas: []string{"claudeshim", "bridging staged skills"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			shimPath := filepath.Join(dir, "claude")
			writeStubRealBinary(t, dir, filepath.Join(dir, "marker.txt"), 7)

			code, _, stderr := runViaHelperProcess(t, shimPath, tc.skillsDir(t), tc.env(t), nil, "")

			assert.Equal(t, 7, code, "a bridge failure must NOT abort the hand-off — the real binary must still have run")
			for _, want := range tc.wantStderrHas {
				assert.Contains(t, stderr, want, "stderr must name the failure loudly, not swallow it")
			}
		})
	}
}
