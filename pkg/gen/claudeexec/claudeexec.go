// Package claudeexec is the shared plumbing for driving the headless `claude`
// CLI to (re)generate a repo artifact. The audit generator (pkg/gen/auditgen)
// builds a prompt and hands it here, so the invocation contract — flags, exec,
// git diff — changes once rather than per generator, and stays in one place if
// a future generator needs it again.
package claudeexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// RunFunc executes the claude binary with the given argv (excluding the binary).
// It is injected into generators so orchestration is testable without a real
// claude process.
type RunFunc func(ctx context.Context, args []string) error

// DiffFunc returns the git diff of code changes for base (the range base...HEAD).
type DiffFunc func(base string) (string, error)

// Args returns the argv (excluding the binary) for a headless claude run. The
// prompt is always the final element. acceptEdits lets the run write its output
// artifact (and, for an audit, the report) without a prompt per file.
func Args(model, prompt string) []string {
	return []string{"-p", "--permission-mode", "acceptEdits", "--model", model, prompt}
}

// Run is the production RunFunc: it streams a real claude invocation to the
// caller's stdout/stderr.
func Run(ctx context.Context, args []string) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return fmt.Errorf("claudeexec: 'claude' not found on PATH (install Claude Code): %w", err)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Diff is the production DiffFunc: `git diff base...HEAD`.
//
// base comes from the environment (DOCS_DIFF_BASE / AUDIT_DIFF_BASE), so it is
// caller-controlled text interpolated into a revision argument.
// --end-of-options stops git's option parsing there, so a base beginning with a
// dash is a (bad) revision rather than a flag: without it, DOCS_DIFF_BASE
// "--output=<path>" makes git write the diff to an arbitrary file. `--` would
// not do here — for git diff it marks the start of PATHSPECS, not the end of
// options, and would make the range a path.
func Diff(base string) (string, error) {
	out, err := exec.Command("git", "diff", "--end-of-options", base+"...HEAD").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff %s...HEAD: %w\n%s", base, err, ee.Stderr)
		}
		return "", fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	return string(out), nil
}
