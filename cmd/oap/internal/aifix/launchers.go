package aifix

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// execCommand builds the *exec.Cmd a launcher runs. It is a package var so tests
// can intercept the exec without spawning a real CLI. (The verify step for this
// feature explicitly does NOT exec real CLIs.)
var execCommand = exec.CommandContext

// cliLauncher is the shared Launcher implementation: every backend differs only
// in its command name and how the prompt is passed on the argv. A new AI CLI is
// a new registration, not a new branch.
type cliLauncher struct {
	name    string
	argsFor func(prompt string) []string
}

func (l cliLauncher) Name() string { return l.name }

func (l cliLauncher) Detect() (string, bool) {
	path, err := lookPath(l.name)
	if err != nil {
		return "", false
	}
	return path, true
}

func (l cliLauncher) Launch(ctx context.Context, prompt string) error {
	path, ok := l.Detect()
	if !ok {
		return fmt.Errorf("%s not found on PATH", l.name)
	}
	cmd := execCommand(ctx, path, l.argsFor(prompt)...)
	// Wire the user's terminal so the launched CLI runs interactively in their
	// own shell with their own credentials.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", l.name, err)
	}
	return nil
}

// positionalArg passes the prompt as a single positional argument — the simplest
// invocation claude and codex both accept (e.g. `claude "<prompt>"`).
func positionalArg(prompt string) []string { return []string{prompt} }

// geminiArgs passes the prompt via -i so the gemini CLI starts interactively
// pre-loaded with it.
func geminiArgs(prompt string) []string { return []string{"-i", prompt} }

func init() {
	Register(cliLauncher{name: "claude", argsFor: positionalArg})
	Register(cliLauncher{name: "codex", argsFor: positionalArg})
	Register(cliLauncher{name: "gemini", argsFor: geminiArgs})
}
