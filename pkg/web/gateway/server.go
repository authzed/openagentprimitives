package gateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements gatewayv1.GatewayServer. It owns no execs of its own: every
// stream it serves was launched and parked by the ToolCall controller, and the
// Registry is the only thing connecting the two.
type Server struct {
	gatewayv1.UnimplementedGatewayServer
	// Registry is where the ToolCall controller parks live execs. A Stream call
	// can only reach an exec that is present here, so a Server with a Registry
	// that no controller writes to serves nothing.
	Registry *Registry

	// helloDeadline bounds the pre-authentication window. Zero means
	// DefaultHelloDeadline; it is a field rather than a bare const read so a
	// test can exercise the expiry without waiting the production budget.
	helloDeadline time.Duration
}

func (s *Server) helloWindow() time.Duration {
	if s.helloDeadline > 0 {
		return s.helloDeadline
	}
	return DefaultHelloDeadline
}

// NewServer returns a Server that serves the streams parked in r. Register it
// with grpc.Server via gatewayv1.RegisterGatewayServer.
func NewServer(r *Registry) *Server {
	return &Server{Registry: r}
}

// Stream serves one client's attachment to one parked exec: it claims the
// stream named in Hello, then pumps the client's stdin into the tool and the
// tool's stdout/stderr back out until the process exits.
//
// The first client message MUST be Hello, or the call fails Unauthenticated. A
// failed claim — unknown key, wrong token, or a stream another client already
// took — is PermissionDenied. On a normal finish it sends exactly one Exit and
// returns nil; if the client disconnects first it cancels the exec and returns
// the context's status rather than leaving the sandbox process unread.
func (s *Server) Stream(stream gatewayv1.Gateway_StreamServer) error {
	ctx := stream.Context()

	// Read Hello, under a DEADLINE.
	//
	// Hello is the only authentication on this stream, and the Recv below is
	// what waits for it — so before it returns, this connection is anonymous.
	// Unbounded, an opened-and-silent stream parked a goroutine and its buffers
	// forever, and grpc-go's own defaults do not help: maxConcurrentStreams is
	// MaxUint32 and connectionTimeout covers only the HTTP/2 handshake, not the
	// application-level wait that follows it.
	//
	// This is the singleton control plane, capped at 256Mi, and its
	// NetworkPolicy admits any pod carrying a label KEY that a pod's own creator
	// supplies. A few thousand silent connections is the whole attack.
	//
	// The bound belongs here rather than in a ServerOption because it is
	// specific to the pre-authentication window: once Hello lands and the claim
	// succeeds, an exec stream is legitimately long-lived and must not inherit
	// a deadline.
	helloCtx, cancelHello := context.WithTimeout(ctx, s.helloWindow())
	defer cancelHello()
	type firstMsg struct {
		msg *gatewayv1.Message
		err error
	}
	firstCh := make(chan firstMsg, 1)
	go func() {
		m, rerr := stream.Recv()
		firstCh <- firstMsg{msg: m, err: rerr}
	}()
	var first *gatewayv1.Message
	var err error
	select {
	case fm := <-firstCh:
		first, err = fm.msg, fm.err
	case <-helloCtx.Done():
		// The Recv goroutine unblocks when the transport tears the stream down
		// on return, and firstCh is buffered so it never leaks on that path.
		return status.Error(codes.DeadlineExceeded, "no Hello within the authentication window")
	}
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "recv hello: %v", err)
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.Unauthenticated, "first message must be Hello")
	}

	active, release, err := s.Registry.Claim(hello.Namespace, hello.ToolCallName, hello.Token)
	if err != nil {
		return status.Errorf(codes.PermissionDenied, "claim: %v", err)
	}
	defer release()

	// grpc-go documents ServerStream.SendMsg as unsafe for concurrent use. The
	// stdout pump, the stderr pump, and this goroutine's final Exit message all
	// target the same stream, so every Send goes through here.
	var sendMu sync.Mutex
	send := func(m *gatewayv1.Message) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}

	errCh := make(chan error, 3)

	// client → tool stdin
	go func() {
		// An interactive ToolCall's stdin is a live pipe (streamingStdin
		// returns nil so the exec layer opens one). Whatever ends the client
		// half — a clean io.EOF, a transport error, a cancelled bridge
		// context — must close it, or the tool blocks on stdin forever, Wait
		// never returns, and the Stream.Close() below is never reached.
		defer func() { _ = active.Stream.Stdin.Close() }()
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
			if chunk := msg.GetStdin(); chunk != nil {
				if _, err := active.Stream.Stdin.Write(chunk.Data); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()

	// tool stdout → client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := active.Stream.Stdout.Read(buf)
			if n > 0 {
				if sendErr := send(&gatewayv1.Message{
					Kind: &gatewayv1.Message_Stdout{
						Stdout: &gatewayv1.BytesChunk{Data: append([]byte(nil), buf[:n]...)},
					},
				}); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	// tool stderr → client
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := active.Stream.Stderr.Read(buf)
			if n > 0 {
				if sendErr := send(&gatewayv1.Message{
					Kind: &gatewayv1.Message_Stderr{
						Stderr: &gatewayv1.BytesChunk{Data: append([]byte(nil), buf[:n]...)},
					},
				}); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if err == io.EOF {
				errCh <- nil
				return
			}
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Wait for the process to exit, then drain goroutines.
	//
	// Stream.Wait selects only on the exec context, which the controller builds
	// from context.Background() with spec.maxDuration or the 4h interactive
	// safety ceiling — never from this gRPC stream's context. A bare Wait()
	// would therefore pin this handler and the sandbox process behind it for
	// hours after the client is gone, so the wait races the stream context.
	type waitOutcome struct {
		result exec.Result
		err    error
	}
	waitCh := make(chan waitOutcome, 1)
	go func() {
		res, err := active.Stream.Wait()
		waitCh <- waitOutcome{result: res, err: err}
	}()

	var result exec.Result
	var waitErr error
	select {
	case out := <-waitCh:
		result, waitErr = out.result, out.err
	case <-ctx.Done():
		// The client is gone. A stream is claimable at most once, so this exec
		// has no possible remaining consumer: stop it rather than leaving a
		// zombie for the exec deadline to reap. Cancel is nil for stream-mode
		// calls, whose own spec.timeout already bounds them.
		if active.Cancel != nil {
			active.Cancel()
		}
		_ = active.Stream.Close()
		return status.FromContextError(ctx.Err()).Err()
	}

	exitMsg := &gatewayv1.Message{
		Kind: &gatewayv1.Message_Exit{
			Exit: &gatewayv1.Exit{Code: result.ExitCode},
		},
	}
	if waitErr != nil {
		exitMsg.GetExit().Reason = waitErr.Error()
	}
	if sendErr := send(exitMsg); sendErr != nil {
		_ = active.Stream.Close()
		return sendErr
	}

	// Close the stream to unblock goroutines reading from the pipes.
	_ = active.Stream.Close()

	// Collect goroutine errors (best-effort; first non-nil wins).
	var firstErr error
	for i := 0; i < 3; i++ {
		select {
		case e := <-errCh:
			if e != nil && firstErr == nil {
				firstErr = e
			}
		case <-ctx.Done():
			firstErr = ctx.Err()
		}
	}
	if firstErr != nil && !errors.Is(firstErr, io.EOF) {
		return status.Errorf(codes.Internal, "stream: %v", firstErr)
	}
	return nil
}

// compile-time guard
var _ gatewayv1.GatewayServer = (*Server)(nil)

// DefaultHelloDeadline bounds the anonymous window: how long a newly-opened stream may
// stay silent before its Hello — the only authentication this service has —
// must have arrived.
//
// Generous relative to the work: the client sends Hello immediately after dial,
// so this is a network-hiccup allowance rather than a budget anything real
// consumes. It bounds only the PRE-authentication wait; an authenticated exec
// stream is legitimately long-lived and inherits nothing from it.
const DefaultHelloDeadline = 30 * time.Second
