package cloud

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// gcloudBin is the gcloud executable name. A package var so tests can point the
// exec helpers at a stand-in (e.g. sh) without a real Cloud SDK install.
var gcloudBin = "gcloud"

// defaultExternalToolTimeout bounds a single external CLI invocation when the
// caller has not set one on the context. It is generous — a GKE node-pool
// recreation runs for many minutes — but finite, so a genuinely wedged tool
// eventually fails instead of hanging until Ctrl-C. Deliberately much larger
// than the install --timeout, which bounds the K8s-side install work, not the
// cloud provider's own LROs. A package var so tests can shrink it.
var defaultExternalToolTimeout = 30 * time.Minute

type externalToolTimeoutKey struct{}

// WithExternalToolTimeout returns a copy of ctx carrying the timeout that
// externalToolCtx (used by Gcloud/GcloudStreaming) applies to external CLI
// subprocess invocations. `oap install` sets this from --external-tool-timeout.
// A zero or negative d is ignored (the default stands). Because the timeout
// travels on the context, it reaches every gcloud call threaded through ctx
// without new function parameters.
func WithExternalToolTimeout(ctx context.Context, d time.Duration) context.Context {
	if d <= 0 {
		return ctx
	}
	return context.WithValue(ctx, externalToolTimeoutKey{}, d)
}

// externalToolTimeoutFrom reads the external-tool timeout from ctx, falling back
// to defaultExternalToolTimeout.
func externalToolTimeoutFrom(ctx context.Context) time.Duration {
	if d, ok := ctx.Value(externalToolTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return defaultExternalToolTimeout
}

// externalToolCtx returns a context for running an external CLI subprocess
// (gcloud). It stays cancelable but is NOT bound by the caller's (short) install
// --timeout deadline — instead it gets its own generous timeout (from the
// context via WithExternalToolTimeout, else defaultExternalToolTimeout).
// External tools drive their own long operations — a GKE workload-identity
// enable or a node-pool recreation runs for many minutes — and an unrelated
// install --timeout must not SIGKILL them mid-flight: that strands the
// operation server-side with no local client watching AND aborts the install
// with a misleading "signal: killed".
//
// It still inherits parent cancellation (SIGINT, or an explicit cancel) so
// Ctrl-C aborts a stuck tool, but ignores the parent's deadline expiring:
// WithoutCancel drops the parent deadline while preserving its values, then we
// apply our own timeout and re-propagate every parent cancellation EXCEPT
// context.DeadlineExceeded. The caller must defer the returned stop().
func externalToolCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), externalToolTimeoutFrom(parent))
	// AfterFunc fires once when parent is Done. Propagate a real cancellation;
	// ignore a bare install --timeout so the long-running tool is not killed by it.
	stopProp := context.AfterFunc(parent, func() {
		if parent.Err() != context.DeadlineExceeded {
			cancel()
		}
	})
	return ctx, func() { stopProp(); cancel() }
}

// Gcloud runs `gcloud <args...>` and returns the combined stdout as a string.
// stderr is reported through rep.Warn on failure so the caller sees gcloud's
// error without needing to inspect a wrapped error string. Used for short,
// output-capturing calls (describe, etc.); long, progress-emitting calls like the
// Gateway API enable use GcloudStreaming so their output isn't buffered and eaten.
//
// Callers that need to check for an "already exists" condition on stderr can
// wrap the returned error with IsGcloudAlreadyExists.
func Gcloud(ctx context.Context, rep Reporter, args ...string) (stdout string, err error) {
	ctx, stop := externalToolCtx(ctx)
	defer stop()
	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, gcloudBin, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if runErr := cmd.Run(); runErr != nil {
		stderrStr := strings.TrimSpace(errBuf.String())
		if stderrStr != "" {
			rep.Warn("gcloud %s: %s", strings.Join(args, " "), stderrStr)
			// Fold stderr into the returned error. cmd.Run() returns an
			// *exec.ExitError whose Error() is only "exit status N" — the real
			// reason (e.g. ALREADY_EXISTS) lives on stderr. Without this, a
			// stderr-matching caller (IsGcloudAlreadyExists) never sees it and an
			// intended-idempotent create surfaces as a hard failure.
			return "", fmt.Errorf("gcloud %s: %w: %s", strings.Join(args, " "), runErr, stderrStr)
		}
		return "", fmt.Errorf("gcloud %s: %w", strings.Join(args, " "), runErr)
	}
	return strings.TrimSpace(outBuf.String()), nil
}

// GcloudStreaming runs a long-running `gcloud <args...>` with stdout AND stderr
// streamed live, so the operator sees gcloud's own progress instead of a silent
// multi-minute hang. It runs inside rep.Suspend so the install checklist
// quiesces and clears its live region; because the writer is the real terminal,
// gcloud detects a TTY and renders its native progress animation.
//
// Suspend also hands us the terminal's stdin, wired to the child. gcloud's
// interactivity check (console_io.IsInteractive) requires stdin to be a TTY
// too; without it gcloud prints one static status line with no spinner and no
// per-poll output, which reads as a hang for the several minutes a
// cluster/node-pool update takes. When the reporter owns no terminal stdin
// (nil), the child's stdin is /dev/null.
//
// Use this for cluster-mutating operations (e.g. enabling the Gateway API);
// short, output-capturing calls use Gcloud.
func GcloudStreaming(ctx context.Context, rep Reporter, args ...string) error {
	ctx, stop := externalToolCtx(ctx)
	defer stop()
	var runErr error
	rep.Suspend(func(out io.Writer, in io.Reader) {
		fmt.Fprintf(out, "$ %s %s\n", gcloudBin, strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, gcloudBin, args...)
		cmd.Stdout = out
		cmd.Stderr = out
		cmd.Stdin = in
		runErr = cmd.Run()
	})
	if runErr != nil {
		return fmt.Errorf("gcloud %s: %w", strings.Join(args, " "), runErr)
	}
	return nil
}

// IsGcloudAlreadyExists reports whether a gcloud error indicates the resource
// already exists. This lets callers treat a redundant "create" as idempotent.
func IsGcloudAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already_exists") || strings.Contains(s, "already exists")
}

// IsGcloudNotFound reports whether a gcloud error indicates the target resource
// does not exist. Lets callers treat a redundant "delete" — or a first-install
// "does this bucket exist yet?" probe — as a clean negative rather than a hard
// failure.
//
// gcloud phrases not-found differently per surface: `container … describe` says
// "The resource 'x' was not found"; storage says "gs://… not found: 404". The
// bare "not found" substring covers both, "not_found" covers the API enum
// style, and ": 404" catches any phrasing leading with the HTTP code.
func IsGcloudNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "not_found") ||
		strings.Contains(s, ": 404")
}
