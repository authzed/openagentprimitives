package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

const RunShellName = "run_shell"
const RunShellSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "cmd": { "type": "string", "description": "command line to execute (must match an allowlist entry)" }
  },
  "required": ["cmd"]
}`

// RunShellConfirmFn is the per-call user-confirmation step. It returns one of
// ShellAllow, ShellDeny, or ShellAlways.
type RunShellConfirmFn func(cmd string, stdin io.Reader, stdout io.Writer) (decision string, err error)

// RunShellConfirm is the package seam for the per-call user confirm step.
// Tests inject a stub; production uses defaultRunShellConfirm.
var RunShellConfirm RunShellConfirmFn = defaultRunShellConfirm

// Decision constants returned by RunShellConfirm.
const (
	// ShellAllow permits this single invocation.
	ShellAllow = "allow"
	// ShellDeny rejects this invocation.
	ShellDeny = "deny"
	// ShellAlways permits every future invocation of the same command in this
	// session, cached in RunShellState.AlwaysApproved.
	ShellAlways = "always"
)

// RunShellState holds the per-session dual-lock state: the compiled provider
// allowlist and the always-approved command cache.
type RunShellState struct {
	// Allowlist is the compiled provider regexes a command must match; empty
	// means run_shell is unavailable for this provider. Patterns must be anchored
	// at both ends — see the security note on CompileAllowlist.
	Allowlist []*regexp.Regexp
	// AlwaysApproved is the per-session set of exact commands the user approved
	// with "always"; nil is treated as empty.
	AlwaysApproved map[string]bool
}

// CompileAllowlist builds compiled regexes from the provider config.
//
// SECURITY: provider-supplied regexes must be anchored at BOTH ends (^ … $).
// regexp.MatchString only requires the pattern to match somewhere in the input,
// so an unanchored "^echo " lets "echo legit; rm -rf /" through the allowlist.
func CompileAllowlist(p *provider.Provider) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(p.RunShellAllowlist))
	for _, a := range p.RunShellAllowlist {
		re, err := regexp.Compile(a.Regex)
		if err != nil {
			return nil, fmt.Errorf("run_shell: compile pattern %q: %w", a.Regex, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// RunShellRun executes a shell command after dual-lock gating:
//  1. Command must match at least one regex in state.Allowlist.
//  2. User must confirm via RunShellConfirm (skipped for always-approved).
//
// exec.CommandContext runs it under a 30-second hard timeout layered over the
// caller's ctx; captured stdout/stderr and the exit code come back as JSON.
func RunShellRun(ctx context.Context, raw json.RawMessage, state *RunShellState,
	stdin io.Reader, stdout io.Writer) (string, error) {

	var args struct {
		// The command line to run; must match an allowlist entry.
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	cmd := strings.TrimSpace(args.Cmd)
	if cmd == "" {
		return "", fmt.Errorf("run_shell: cmd required")
	}
	if state == nil || len(state.Allowlist) == 0 {
		return "", fmt.Errorf("run_shell: no allowlist configured for this provider")
	}
	if !matchesAny(cmd, state.Allowlist) {
		return "", fmt.Errorf("run_shell: %q does not match provider allowlist", cmd)
	}

	// Per-call confirm — skipped if the command is already always-approved.
	if state.AlwaysApproved == nil {
		state.AlwaysApproved = map[string]bool{}
	}
	if !state.AlwaysApproved[cmd] {
		decision, err := RunShellConfirm(cmd, stdin, stdout)
		if err != nil {
			return "", err
		}
		switch decision {
		case ShellAllow:
			// single-use permit; do not cache
		case ShellAlways:
			state.AlwaysApproved[cmd] = true
		case ShellDeny:
			return "", fmt.Errorf("run_shell: user denied")
		default:
			return "", fmt.Errorf("run_shell: unknown decision %q", decision)
		}
	}

	// Execute with a 30-second hard timeout layered over ctx.
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(execCtx, "sh", "-c", cmd)
	var stdoutBuf, stderrBuf bytes.Buffer
	c.Stdout = &stdoutBuf
	c.Stderr = &stderrBuf
	runErr := c.Run()
	exit := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return "", fmt.Errorf("run_shell: %w", runErr)
		}
	}
	out := struct {
		// Captured stdout, truncated at 8 KiB with a marker.
		Stdout string `json:"stdout"`
		// Captured stderr, truncated at 8 KiB with a marker.
		Stderr string `json:"stderr"`
		// The command's exit code; 0 when it succeeded.
		Exit int `json:"exit"`
	}{
		Stdout: trimOutput(stdoutBuf.String()),
		Stderr: trimOutput(stderrBuf.String()),
		Exit:   exit,
	}
	j, _ := json.Marshal(out)
	return string(j), nil
}

func matchesAny(s string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func trimOutput(s string) string {
	const max = 8 * 1024
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// defaultRunShellConfirm is the production confirm: it reads y/N/always from
// stdin and returns the matching decision constant.
func defaultRunShellConfirm(cmd string, stdin io.Reader, stdout io.Writer) (string, error) {
	fmt.Fprintf(stdout, "Setup wants to run: %s\nAllow once? [y/N/always-this-session]: ", cmd)
	var resp string
	if _, err := fmt.Fscanln(stdin, &resp); err != nil && err.Error() != "unexpected newline" {
		return ShellDeny, nil
	}
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "y", "yes":
		return ShellAllow, nil
	case "always", "a", "always-this-session":
		return ShellAlways, nil
	default:
		return ShellDeny, nil
	}
}
