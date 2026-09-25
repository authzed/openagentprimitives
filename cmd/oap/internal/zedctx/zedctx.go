// Package zedctx is a thin wrapper around the `zed` CLI's context management
// subcommands (set / use / remove). Used by `oap spicedb proxy` to configure
// a local zed context that points at the in-cluster SpiceDB via a port-forward.
package zedctx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// Runner shells out to `zed`. Construct with NewRunner for production use;
// tests use NewRunnerForTest to inject a captured run function.
type Runner struct {
	// path is the binary name or absolute path. NewRunner sets this to "zed";
	// the caller can pass a different name for tests.
	path string
	// runFn is invoked instead of (*exec.Cmd).CombinedOutput. Non-nil only in
	// tests. Production code path runs the real command.
	runFn func(*exec.Cmd) ([]byte, error)
}

// NewRunner returns a Runner that invokes `zed` from PATH.
func NewRunner() *Runner {
	return &Runner{path: "zed"}
}

// NewRunnerForTest constructs a Runner that calls fn instead of executing the
// real binary. Intended for unit tests only.
func NewRunnerForTest(path string, fn func(*exec.Cmd) ([]byte, error)) *Runner {
	return &Runner{path: path, runFn: fn}
}

// Available reports whether the zed binary is present on PATH. Returns true
// when a Runner was constructed via NewRunnerForTest (tests don't need a
// real binary).
func (r *Runner) Available() bool {
	if r.runFn != nil {
		return true
	}
	_, err := exec.LookPath(r.path)
	return err == nil
}

// Set writes a zed context entry. token is the SpiceDB pre-shared key — the
// unscoped root credential for the entire authorization system — so it is
// masked out of any error this produces.
//
// It still reaches zed verbatim in the executed ARGV, and argv is world-visible
// through `ps` for the duration of the process. That is a real exposure and it
// is accepted rather than overlooked:
//
//   - `zed context set` takes the token as a positional argument. It is a
//     third-party CLI and there is no stdin or environment form of that
//     subcommand to switch to, so this cannot be fixed here — only by not
//     shelling out to zed at all.
//   - The audience is other LOCAL users on the machine running `oap`, during a
//     process that exits in milliseconds. On a developer laptop that audience
//     is empty; on a shared box it is not, and anyone in it can already read
//     the kubeconfig this command was driven from, which reaches the same
//     cluster by a different door.
//
// The masking below covers the other half — the error text, which persists in
// logs and terminal scrollback long after the process is gone.
func (r *Runner) Set(ctx context.Context, name, endpoint, token string, insecure bool) error {
	args := []string{"context", "set", name, endpoint, token}
	if insecure {
		args = append(args, "--insecure")
	}
	// Build the argv the error message may quote, with the token replaced in
	// place. Masking here rather than inside run keeps run ignorant of which
	// positional argument is secret — Set is the only method that has one.
	safe := append([]string(nil), args...)
	safe[4] = credmask.Mask(token)
	return r.run(ctx, safe, args...)
}

func (r *Runner) Use(ctx context.Context, name string) error {
	args := []string{"context", "use", name}
	return r.run(ctx, args, args...)
}

func (r *Runner) Remove(ctx context.Context, name string) error {
	args := []string{"context", "remove", name}
	return r.run(ctx, args, args...)
}

// run executes zed with args and reports failures using safeArgs in the
// message. The two are separate so a secret positional argument can be
// redacted from operator-visible output without altering what is executed;
// callers with no secret pass the same slice for both.
func (r *Runner) run(ctx context.Context, safeArgs []string, args ...string) error {
	if r.path == "" {
		return errors.New("zedctx: empty path")
	}
	cmd := exec.CommandContext(ctx, r.path, args...)
	var (
		out []byte
		err error
	)
	if r.runFn != nil {
		out, err = r.runFn(cmd)
	} else {
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return fmt.Errorf("zed %v failed: %s: %w", safeArgs, string(out), err)
	}
	return nil
}
