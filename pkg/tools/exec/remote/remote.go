// Package remote implements exec.Executor against the Kubernetes API server
// via the pods/exec subresource. A Binder holds the cluster connection; its
// For method binds an Executor to one pod container.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// Binder creates executors bound to a single pod container. One Binder serves
// a whole cluster; each Executor it yields serves exactly one sandbox.
type Binder struct {
	cfg       *rest.Config
	clientset kubernetes.Interface
}

// newSPDYExecutor is the seam used to build the underlying streaming executor.
// It defaults to client-go's real SPDY executor; tests override it to inject a
// fake that returns specific transport errors (e.g. a non-ExitStatus SPDY drop)
// without standing up an API server.
var newSPDYExecutor = func(cfg *rest.Config, method string, u *url.URL) (remotecommand.Executor, error) {
	return remotecommand.NewSPDYExecutor(cfg, method, u)
}

// New returns a Binder backed by the given REST config.
func New(cfg *rest.Config) (*Binder, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Binder{cfg: cfg, clientset: cs}, nil
}

// For returns an executor bound to one pod container. The target is fixed at
// construction, so a caller cannot address a different pod through it.
func (b *Binder) For(namespace, pod, container string) exec.Executor {
	return &boundExecutor{binder: b, namespace: namespace, pod: pod, container: container}
}

// boundExecutor runs commands against the one pod container it was created
// for via Binder.For. Nothing in an exec.Request can redirect it elsewhere.
type boundExecutor struct {
	binder    *Binder
	namespace string
	pod       string
	container string
}

func (e *boundExecutor) Exec(ctx context.Context, req exec.Request) (exec.Result, error) {
	if len(req.Command) == 0 {
		return exec.Result{ExitCode: -1}, errors.New("remote: command must not be empty")
	}

	stdout := &boundedBuffer{limit: exec.MaxStreamBytes}
	stderr := &boundedBuffer{limit: exec.MaxStreamBytes}

	// Env is delivered through a per-call file, never through argv — argv is
	// query-string material the apiserver audits and /proc exposes. See env.go.
	cmd, cleanupEnv, err := e.withEnv(ctx, req)
	if err != nil {
		return exec.Result{ExitCode: -1}, err
	}

	// The staged file is unlinked by the loader in the TOOL's own argv, so only
	// a remote process that reported an exit status proves it is already gone.
	// Every other way out of this function — a build failure, a transport drop,
	// a ctx deadline — has to unlink it here or leave a plaintext credential
	// readable to every later tool call in the sandbox. See env.go.
	toolReportedExit := false
	defer func() {
		if !toolReportedExit {
			cleanupEnv()
		}
	}()

	opts := &corev1.PodExecOptions{
		Container: e.container,
		Command:   cmd,
		Stdin:     req.Stdin != nil,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}

	restReq := e.binder.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(e.namespace).
		Name(e.pod).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec)

	rc, err := newSPDYExecutor(e.binder.cfg, "POST", restReq.URL())
	if err != nil {
		return exec.Result{ExitCode: -1}, fmt.Errorf("remote: build executor: %w", err)
	}

	streamErr := rc.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  req.Stdin,
		Stdout: stdout,
		Stderr: stderr,
	})

	result := exec.Result{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}

	if streamErr == nil {
		result.ExitCode = 0
		toolReportedExit = true
		return result, nil
	}

	// The remotecommand package returns k8s.io/client-go/util/exec.CodeExitError
	// (a value type) to carry the remote process exit code. Because it is not a
	// pointer receiver, errors.As(*CodeExitError) does not match; we use a
	// duck-typed interface assertion instead.
	if ee, ok := streamErr.(interface{ ExitStatus() int }); ok {
		result.ExitCode = int32(ee.ExitStatus())
		toolReportedExit = true
		return result, nil
	}

	result.ExitCode = -1
	return result, streamErr
}

// boundedBuffer captures up to limit bytes; further writes are counted but discarded.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remain := b.limit - b.buf.Len()
	if remain <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remain {
		b.buf.Write(p[:remain])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// Compile-time interface check.
var _ exec.Executor = (*boundExecutor)(nil)

// Ensure we still depend on io for the interface contract.
var _ io.Writer = (*boundedBuffer)(nil)

// StreamExec implements exec.StreamingExecutor.
func (e *boundExecutor) StreamExec(ctx context.Context, req exec.Request) (*exec.Stream, error) {
	if len(req.Command) == 0 {
		return nil, fmt.Errorf("remote: command must not be empty")
	}

	// Same per-call env file as Exec, staged through the same helper — the
	// streaming path carries the identical credentials for stream-mode and
	// interactive ToolCalls, so it must not have a delivery shape of its own.
	cmd, cleanupEnv, err := e.withEnv(ctx, req)
	if err != nil {
		return nil, err
	}

	// Cleanup ownership differs from Exec's only in WHO holds it: the tool's
	// exec outlives this function, so once the stream goroutine below is
	// running, unlinking the staged file is its job. Until then an early return
	// here is the only thing that can, and a fix covering just Exec would leave
	// every interactive ToolCall orphaning credentials. See env.go.
	streamOwnsCleanup := false
	defer func() {
		if !streamOwnsCleanup {
			cleanupEnv()
		}
	}()

	opts := &corev1.PodExecOptions{
		Container: e.container,
		Command:   cmd,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}

	restReq := e.binder.clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(e.namespace).
		Name(e.pod).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec)

	rc, err := newSPDYExecutor(e.binder.cfg, "POST", restReq.URL())
	if err != nil {
		return nil, fmt.Errorf("remote: build executor: %w", err)
	}

	// stdin wiring depends on whether the caller supplied a finite source.
	//
	//   - req.Stdin != nil: a finite reader (e.g. stream-mode ToolCalls pass
	//     an empty bytes.Reader). The kubelet drains it and the tool sees
	//     stdin at EOF immediately — no open never-written pipe, so a
	//     one-shot tool like `claude --print` does not stall. Stream.Stdin is
	//     then a no-op closed writer; there is no live pipe to write into.
	//   - req.Stdin == nil: open a live pipe. Stream.Stdin is the pipe writer
	//     the gateway bridge feeds with channel input (interactive ToolCalls).
	var (
		toolStdin io.Reader
		stdinW    io.WriteCloser
		stdinR    io.Closer
	)
	if req.Stdin != nil {
		toolStdin = req.Stdin
		stdinW = closedNopWriteCloser{}
	} else {
		pr, pw := io.Pipe()
		toolStdin, stdinW, stdinR = pr, pw, pr
	}
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// streamOutcome carries both the Result and any transport error so Wait can
	// surface a non-ExitStatus failure (SPDY drop, auth failure, pod-gone) to
	// the caller — matching synchronous Exec, which returns streamErr. Without
	// this, such a failure was mapped to ExitCode=-1 with a nil error and the
	// underlying cause was discarded.
	type streamOutcome struct {
		res exec.Result
		err error
	}
	exitC := make(chan streamOutcome, 1)

	streamOwnsCleanup = true
	go func() {
		streamErr := rc.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:  toolStdin,
			Stdout: stdoutW,
			Stderr: stderrW,
		})
		_ = stdoutW.Close()
		_ = stderrW.Close()
		oc := streamOutcome{}
		if streamErr == nil {
			oc.res.ExitCode = 0
		} else if ee, ok := streamErr.(interface{ ExitStatus() int }); ok {
			oc.res.ExitCode = int32(ee.ExitStatus())
		} else {
			// Non-ExitStatus transport error: the process never reported an
			// exit code. Report -1 and surface the cause via Wait's error.
			oc.res.ExitCode = -1
			oc.err = streamErr
		}
		exitC <- oc
		if oc.err != nil {
			// No exit status means the loader may never have run, so the staged
			// env file may still be there. Unlink AFTER publishing the outcome:
			// Wait must not be delayed by a cleanup round trip.
			cleanupEnv()
		}
	}()

	// Wait must be safe to call from multiple consumers (the controller's
	// stream watcher and the gateway server both call it for the same
	// streaming ToolCall). The Once ensures the underlying channel is
	// drained exactly once; subsequent callers see the cached result.
	var (
		waitOnce sync.Once
		waitRes  exec.Result
		waitErr  error
	)
	wait := func() (exec.Result, error) {
		waitOnce.Do(func() {
			select {
			case oc := <-exitC:
				waitRes = oc.res
				waitErr = oc.err
			case <-ctx.Done():
				waitRes = exec.Result{ExitCode: -1}
				waitErr = ctx.Err()
			}
		})
		return waitRes, waitErr
	}

	closer := func() error {
		_ = stdinW.Close()
		if stdinR != nil { // nil when req.Stdin was a finite caller-owned reader
			_ = stdinR.Close()
		}
		_ = stdoutR.Close()
		_ = stderrR.Close()
		return nil
	}

	return &exec.Stream{
		Stdin:  stdinW,
		Stdout: stdoutR,
		Stderr: stderrR,
		Wait:   wait,
		Close:  closer,
	}, nil
}

// Compile-time check.
var _ exec.StreamingExecutor = (*boundExecutor)(nil)

// closedNopWriteCloser is the Stream.Stdin returned when StreamExec was given
// a finite Request.Stdin: there is no live pipe to write into. Writes report
// stdin is already closed; Close is a no-op.
type closedNopWriteCloser struct{}

func (closedNopWriteCloser) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (closedNopWriteCloser) Close() error              { return nil }
