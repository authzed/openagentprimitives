package gateway

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// fakeStreamServer is a Gateway_StreamServer whose Recv is scripted and whose
// Send models the one property grpc-go actually guarantees about a
// ServerStream: SendMsg must not be called concurrently. It records whether
// two goroutines were ever inside Send at the same time, holding each call open
// for sendHold so a second sender has a real chance to arrive.
//
// grpc.ServerStream is embedded as a nil interface deliberately: any method the
// handler calls that this fake does not implement is a panic, which is the
// loudest possible signal that the handler grew a dependency the test does not
// model.
type fakeStreamServer struct {
	grpc.ServerStream

	ctx      context.Context
	recv     func() (*gatewayv1.Message, error)
	sendHold time.Duration

	mu         sync.Mutex
	depth      int
	overlapped bool
	sent       []*gatewayv1.Message
}

func (f *fakeStreamServer) Context() context.Context { return f.ctx }

func (f *fakeStreamServer) Recv() (*gatewayv1.Message, error) { return f.recv() }

func (f *fakeStreamServer) Send(m *gatewayv1.Message) error {
	f.mu.Lock()
	f.depth++
	if f.depth > 1 {
		f.overlapped = true
	}
	f.mu.Unlock()

	if f.sendHold > 0 {
		time.Sleep(f.sendHold)
	}

	f.mu.Lock()
	f.depth--
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}

func (f *fakeStreamServer) sawOverlappingSends() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overlapped
}

// helloThen returns a Recv func that yields the Hello handshake once, then
// whatever next returns on every subsequent call.
func helloThen(namespace, name, token string, next func() (*gatewayv1.Message, error)) func() (*gatewayv1.Message, error) {
	var once sync.Once
	return func() (*gatewayv1.Message, error) {
		var hello *gatewayv1.Message
		once.Do(func() {
			hello = &gatewayv1.Message{Kind: &gatewayv1.Message_Hello{Hello: &gatewayv1.Hello{
				Namespace: namespace, ToolCallName: name, Token: token,
			}}}
		})
		if hello != nil {
			return hello, nil
		}
		return next()
	}
}

// registerClaimable registers an ActiveStream carrying st and returns the raw
// token a client presents to claim it.
func registerClaimable(t *testing.T, reg *Registry, name string, st *exec.Stream, cancel context.CancelFunc) string {
	t.Helper()
	const rawToken = "server-test-raw-token"
	require.NoError(t, reg.Register(&ActiveStream{
		Namespace:    "default",
		ToolCallName: name,
		TokenHash:    sha256hex(rawToken),
		Stream:       st,
		Cancel:       cancel,
	}), "Register")
	return rawToken
}

// TestServerStream_NonEOFRecvError_ClosesToolStdin asserts the tool's stdin is
// closed when the client half dies for ANY reason, not just a clean io.EOF.
//
// An interactive ToolCall's stdin is a live pipe (streamingStdin returns nil so
// the exec layer opens one). A runner restart, a network blip, or the bridge's
// context being cancelled mid-turn surfaces here as codes.Canceled — not
// io.EOF. Leaving the pipe open blocks the tool on stdin forever, which means
// Wait() never returns, which means the Stream.Close() at the bottom of the
// handler is never reached either.
func TestServerStream_NonEOFRecvError_ClosesToolStdin(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// The tool exits once its stdin reaches EOF — exactly what a real tool does.
	exited := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, stdinR)
		close(exited)
		_ = stdoutW.Close()
		_ = stderrW.Close()
	}()

	st := &exec.Stream{
		Stdin: stdinW, Stdout: stdoutR, Stderr: stderrR,
		Wait: func() (exec.Result, error) {
			<-exited
			return exec.Result{ExitCode: 0}, nil
		},
		Close: func() error { return nil },
	}

	reg := NewRegistry()
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	token := registerClaimable(t, reg, "tc-recv-err", st, cancel)

	fs := &fakeStreamServer{
		ctx: context.Background(),
		recv: helloThen("default", "tc-recv-err", token, func() (*gatewayv1.Message, error) {
			// Not io.EOF: the transport-level failure a restart or a
			// cancelled bridge context produces.
			return nil, context.Canceled
		}),
	}

	done := make(chan error, 1)
	go func() { done <- NewServer(reg).Stream(fs) }()

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("tool stdin was never closed after a non-EOF Recv error: an interactive tool blocks on stdin forever and Wait() never returns")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the tool exited")
	}
}

// TestServerStream_StdoutAndStderr_NeverSendConcurrently asserts the handler
// serializes writes to the gRPC stream. grpc-go documents ServerStream.SendMsg
// as unsafe for concurrent use; the stdout pump, the stderr pump, and the final
// Exit message all target the same stream, and a losing first Send races into
// ErrIllegalHeaderWrite, which terminates a live interactive session with a
// spurious Internal status.
func TestServerStream_StdoutAndStderr_NeverSendConcurrently(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()

	// Both pumps have data waiting before the handler starts reading, so they
	// reach Send at effectively the same instant.
	ready := make(chan struct{})
	go func() {
		<-ready
		_, _ = stdoutW.Write([]byte("out"))
		_ = stdoutW.Close()
	}()
	go func() {
		<-ready
		_, _ = stderrW.Write([]byte("err"))
		_ = stderrW.Close()
	}()

	drained := make(chan struct{})
	st := &exec.Stream{
		Stdin: stdinW, Stdout: stdoutR, Stderr: stderrR,
		Wait: func() (exec.Result, error) {
			<-drained
			return exec.Result{ExitCode: 0}, nil
		},
		Close: func() error { return nil },
	}

	reg := NewRegistry()
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	token := registerClaimable(t, reg, "tc-send-race", st, cancel)

	fs := &fakeStreamServer{
		ctx:      context.Background(),
		sendHold: 50 * time.Millisecond,
		recv: helloThen("default", "tc-send-race", token, func() (*gatewayv1.Message, error) {
			<-drained
			return nil, io.EOF
		}),
	}

	done := make(chan error, 1)
	go func() { done <- NewServer(reg).Stream(fs) }()
	close(ready)

	// Give both pumps time to reach Send, then let the process exit.
	time.Sleep(200 * time.Millisecond)
	close(drained)

	select {
	case err := <-done:
		assert.NoError(t, err, "handler completes cleanly")
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	assert.False(t, fs.sawOverlappingSends(),
		"stdout, stderr, and the Exit message must not call Send concurrently — grpc-go forbids concurrent SendMsg on one ServerStream")
}

// TestServerStream_ClientDisconnect_ReturnsAndCancelsExec asserts a gone client
// releases the handler AND stops the exec.
//
// Stream.Wait selects only on the exec context, which reconcileStreaming builds
// from context.Background() with spec.maxDuration or the 4h interactive
// ceiling — never from the gRPC stream context. A bare Wait() therefore pins
// this handler goroutine and the sandbox process behind it for hours after the
// client is gone. Because a stream can never be re-claimed, a gone client means
// the exec has no possible remaining consumer.
func TestServerStream_ClientDisconnect_ReturnsAndCancelsExec(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	t.Cleanup(func() { _ = stdoutW.Close(); _ = stderrW.Close() })
	go func() { _, _ = io.Copy(io.Discard, stdinR) }()

	execCtx, cancelExec := context.WithCancel(context.Background())
	t.Cleanup(cancelExec)

	closed := make(chan struct{})
	var closeOnce sync.Once
	st := &exec.Stream{
		Stdin: stdinW, Stdout: stdoutR, Stderr: stderrR,
		// A live interactive session: the process runs until its exec context
		// is cancelled, which is what the 4h ceiling or a controller-side
		// cancel does.
		Wait: func() (exec.Result, error) {
			<-execCtx.Done()
			return exec.Result{ExitCode: -1}, execCtx.Err()
		},
		Close: func() error { closeOnce.Do(func() { close(closed) }); return nil },
	}

	reg := NewRegistry()
	token := registerClaimable(t, reg, "tc-disconnect", st, cancelExec)

	clientCtx, disconnect := context.WithCancel(context.Background())
	fs := &fakeStreamServer{
		ctx: clientCtx,
		recv: helloThen("default", "tc-disconnect", token, func() (*gatewayv1.Message, error) {
			<-clientCtx.Done()
			return nil, context.Canceled
		}),
	}

	done := make(chan error, 1)
	go func() { done <- NewServer(reg).Stream(fs) }()

	time.Sleep(50 * time.Millisecond)
	disconnect()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler stayed blocked in Stream.Wait after the client disconnected — it holds the sandbox process until the exec deadline (up to 4h)")
	}
	assert.Error(t, execCtx.Err(), "a disconnected client leaves the exec with no possible consumer; it must be cancelled")
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Stream.Close was never called on the disconnect path")
	}
}
