// Package fake provides an in-process fake for exec.Executor, used by
// controller and e2e tests. A Binder holds every scripted response; its For
// method binds an Executor to one sandbox target, mirroring
// pkg/tools/exec/remote.Binder so the two transports are interchangeable at
// construction sites.
package fake

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// Response is what the fake returns for a given programmed key.
type Response struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int32
	Err      error
	// Delay introduces a wait before returning. Cancel via ctx still works.
	Delay time.Duration
}

// Call records one invocation for post-hoc assertions.
type Call struct {
	// Namespace, Pod and Container are the target the executor was bound to.
	// They live here rather than on Request because a Request carries no
	// identity — the executor does.
	Namespace string
	Pod       string
	Container string
	Request   exec.Request
	Stdin     []byte
}

// Binder holds every response programmed via Program, ProgramFunc,
// ProgramStream and ProgramStreamFunc, plus every Call recorded across all
// executors it has yielded. One Binder serves a whole test; each Executor its
// For method returns is bound to one sandbox target.
type Binder struct {
	mu             sync.Mutex
	programs       map[string]Response
	programFuncs   map[string]func(exec.Request) Response
	streamPrograms map[string]StreamResponse
	streamFuncs    map[string]func(stdin io.Reader, stdout, stderr io.Writer) int32
	calls          []Call
}

// New returns an empty Binder.
func New() *Binder {
	return &Binder{
		programs:       map[string]Response{},
		programFuncs:   map[string]func(exec.Request) Response{},
		streamPrograms: map[string]StreamResponse{},
		streamFuncs:    map[string]func(stdin io.Reader, stdout, stderr io.Writer) int32{},
	}
}

// Program registers a Response for a "namespace/pod:container" key.
//
// Every command run against that key gets the SAME answer. That is all a
// scenario driving one tool needs; see ProgramFunc for the case where it is
// not.
func (b *Binder) Program(key string, resp Response) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.programs[key] = resp
}

// ProgramFunc registers a request-inspecting responder for a
// "namespace/pod:container" key.
//
// A key names a POD, not a tool, and one sandbox pod runs every tool its
// SpiceboxClass declares — so two tools sharing a class share a key and cannot
// be told apart by Program, which answers a key with one static Response. The
// Request the executor already receives carries the argv, which is what
// distinguishes them.
//
// The dispatch RULE stays with the caller rather than moving in here: only the
// caller knows how a command maps onto a tool (this package has no idea a
// SpiceboxClass exists), and a matching rule baked in here would be a second
// place that has to agree with the class catalog. This package's job is to hand
// the responder the request.
//
// A responder is expected to answer an unrecognized command with an explicit
// failure Response naming it — a silent empty result would replay as a tool
// that ran and produced nothing.
//
// WINS over Program for the same key, mirroring ProgramStreamFunc's precedence
// over ProgramStream. Registering both is a caller deciding the func is the
// answer, not a merge.
func (b *Binder) ProgramFunc(key string, fn func(exec.Request) Response) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.programFuncs[key] = fn
}

// Calls returns the invocations observed so far, across every executor this
// Binder has yielded via For.
func (b *Binder) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Call, len(b.calls))
	copy(out, b.calls)
	return out
}

// For returns an executor bound to one sandbox target. The target is fixed
// at construction, so a caller cannot address a different sandbox through
// it. It composes the same "<namespace>/<pod>:<container>" key that
// Program/ProgramStream/ProgramStreamFunc are keyed by, so every existing
// registration is found without the Request supplying identity.
func (b *Binder) For(namespace, pod, container string) exec.Executor {
	return &boundExecutor{binder: b, namespace: namespace, pod: pod, container: container}
}

// boundExecutor runs commands against the one sandbox target it was created
// for via Binder.For. Nothing in an exec.Request can redirect it elsewhere.
type boundExecutor struct {
	binder    *Binder
	namespace string
	pod       string
	container string
}

// key composes the lookup key a bound executor uses against its Binder's
// program maps: "<namespace>/<pod>:<container>".
func (e *boundExecutor) key() string {
	return fmt.Sprintf("%s/%s:%s", e.namespace, e.pod, e.container)
}

func (e *boundExecutor) Exec(ctx context.Context, req exec.Request) (exec.Result, error) {
	key := e.key()

	var stdin []byte
	if req.Stdin != nil {
		var err error
		stdin, err = io.ReadAll(req.Stdin)
		if err != nil {
			return exec.Result{ExitCode: -1}, err
		}
	}

	e.binder.mu.Lock()
	fn, hasFunc := e.binder.programFuncs[key]
	resp, hasResp := e.binder.programs[key]
	e.binder.calls = append(e.binder.calls, Call{
		Namespace: e.namespace,
		Pod:       e.pod,
		Container: e.container,
		Request:   req,
		Stdin:     stdin,
	})
	e.binder.mu.Unlock()

	// The responder runs OUTSIDE the binder lock: it is caller code that may do
	// anything, and holding a lock the caller could re-enter through Calls or
	// Program would deadlock the whole test.
	if hasFunc {
		resp = fn(req)
	} else if !hasResp {
		return exec.Result{ExitCode: -1}, fmt.Errorf("fake: no program for %q", key)
	}

	if resp.Delay > 0 {
		select {
		case <-ctx.Done():
			return exec.Result{ExitCode: -1}, ctx.Err()
		case <-time.After(resp.Delay):
		}
	}

	if resp.Err != nil {
		return exec.Result{ExitCode: -1}, resp.Err
	}

	return exec.Result{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: resp.ExitCode,
	}, nil
}

// Compile-time interface check.
var _ exec.Executor = (*boundExecutor)(nil)

// StreamResponse is the streaming-mode analog of Response.
type StreamResponse struct {
	// Stdout/Stderr are the bytes the fake emits after accepting stdin.
	Stdout []byte
	Stderr []byte
	// ExitCode reported by Wait.
	ExitCode int32
	// OnStdin, if non-nil, is invoked with the bytes the caller wrote to the stream's stdin.
	OnStdin func([]byte)
}

// ProgramStream registers a streaming response for a key.
func (b *Binder) ProgramStream(key string, resp StreamResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.streamPrograms[key] = resp
}

// ProgramStreamFunc registers a driver function for a "namespace/pod:container"
// key. When StreamExec is called for that key, the driver is invoked with the
// stream's stdin/stdout/stderr pipes and runs until it returns; the returned
// value is the exit code reported by Wait.
//
// Use this for interactive fakes that need to react to stdin chunks
// (ScriptedInteractiveTool in test/e2e). For one-shot canned output, prefer
// ProgramStream.
func (b *Binder) ProgramStreamFunc(key string, driver func(stdin io.Reader, stdout, stderr io.Writer) int32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.streamFuncs[key] = driver
}

// StreamExec implements exec.StreamingExecutor.
func (e *boundExecutor) StreamExec(ctx context.Context, req exec.Request) (*exec.Stream, error) {
	key := e.key()

	e.binder.mu.Lock()
	driver, hasFunc := e.binder.streamFuncs[key]
	resp, hasResp := e.binder.streamPrograms[key]
	e.binder.calls = append(e.binder.calls, Call{
		Namespace: e.namespace,
		Pod:       e.pod,
		Container: e.container,
		Request:   req,
	})
	e.binder.mu.Unlock()
	if !hasFunc && !hasResp {
		return nil, fmt.Errorf("fake: no stream program for %q", key)
	}

	// stdin wiring mirrors remote.StreamExec: a non-nil Request.Stdin is a
	// finite source the tool drains to EOF (Stream.Stdin is then a no-op
	// closed writer); a nil Request.Stdin opens a live pipe the caller writes
	// to via Stream.Stdin.
	var (
		toolStdin io.Reader
		stdinW    io.WriteCloser
	)
	if req.Stdin != nil {
		toolStdin = req.Stdin
		stdinW = closedNopWriteCloser{}
	} else {
		pr, pw := io.Pipe()
		toolStdin, stdinW = pr, pw
	}
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	exitC := make(chan exec.Result, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		if hasFunc {
			code := driver(toolStdin, stdoutW, stderrW)
			_ = stdoutW.Close()
			_ = stderrW.Close()
			exitC <- exec.Result{ExitCode: code}
			return
		}
		// Consume stdin if any handler is interested; otherwise drain it.
		buf, _ := io.ReadAll(toolStdin)
		if resp.OnStdin != nil {
			resp.OnStdin(buf)
		}
		// Emit stdout/stderr, then close each writer so readers see EOF.
		if len(resp.Stdout) > 0 {
			_, _ = stdoutW.Write(resp.Stdout)
		}
		_ = stdoutW.Close()
		if len(resp.Stderr) > 0 {
			_, _ = stderrW.Write(resp.Stderr)
		}
		_ = stderrW.Close()
		exitC <- exec.Result{ExitCode: resp.ExitCode, Stdout: resp.Stdout, Stderr: resp.Stderr}
	}()

	// Wait is idempotent — multiple consumers (controller watcher + gateway
	// server) call it for the same streaming ToolCall.
	var (
		waitOnce sync.Once
		waitRes  exec.Result
		waitErr  error
	)
	wait := func() (exec.Result, error) {
		waitOnce.Do(func() {
			select {
			case r := <-exitC:
				waitRes = r
			case <-ctx.Done():
				waitRes = exec.Result{ExitCode: -1}
				waitErr = ctx.Err()
			}
		})
		return waitRes, waitErr
	}

	closer := func() error {
		_ = stdinW.Close()
		// Close the tool-side stdin reader too when it is a pipe; a finite
		// Request.Stdin reader may not be a Closer and needs none.
		if rc, ok := toolStdin.(io.Closer); ok {
			_ = rc.Close()
		}
		_ = stdoutR.Close()
		_ = stderrR.Close()
		<-done
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

// closedNopWriteCloser is the Stream.Stdin returned when Request.Stdin is a
// finite reader: there is no live pipe to write into. Writes report stdin is
// already closed; Close is a no-op.
type closedNopWriteCloser struct{}

func (closedNopWriteCloser) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (closedNopWriteCloser) Close() error              { return nil }
