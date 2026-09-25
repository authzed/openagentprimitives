package sandbox

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// BridgeConfig configures a gateway-client bridge for one interactive ToolCall.
type BridgeConfig struct {
	GatewayAddr  string
	Namespace    string
	ToolCallName string
	Token        string
	// IdleTimeout ends the session after this much inactivity (no output and
	// no input). Zero disables the idle timer.
	IdleTimeout time.Duration
	// OnOutput is called for every stdout/stderr chunk the tool emits.
	OnOutput func(stream string, data []byte)
	// OnIdle is called once when the idle timer fires. The bridge keeps
	// running; the caller is expected to react (e.g. delete the ToolCall CR).
	OnIdle func()
}

// BridgeResult is the terminal outcome of a bridged interactive session.
type BridgeResult struct {
	ExitCode   int32
	ExitReason string // "completed" | "idle" | "maxDuration" | "failed"
}

// Bridge claims a gateway stream and pumps it bidirectionally.
type Bridge struct {
	cfg    BridgeConfig
	mu     sync.Mutex
	stream gatewayv1.Gateway_StreamClient
	idle   *time.Timer
	idled  bool
	// dialOpts allows tests to inject a bufconn-based dialer. Production callers
	// leave it nil and the bridge dials cfg.GatewayAddr with insecure creds.
	dialOpts []grpc.DialOption
	// wrapStream, when non-nil, wraps the gateway stream immediately after it is
	// opened. Production callers leave it nil. Tests use it to observe the order
	// of the Send / CloseSend calls the bridge makes: grpc-go forbids the two
	// from overlapping, and that ban is only observable at the stream boundary.
	wrapStream func(gatewayv1.Gateway_StreamClient) gatewayv1.Gateway_StreamClient
}

// NewBridge constructs a Bridge but does not connect; call Run to start.
func NewBridge(cfg BridgeConfig) *Bridge { return &Bridge{cfg: cfg} }

// Feed writes data to the tool's stdin and resets the idle timer. Safe for
// concurrent use; returns an error if the stream is not open.
func (b *Bridge) Feed(data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stream == nil {
		return errors.New("bridge: stream not open")
	}
	b.resetIdleLocked()
	return b.stream.Send(&gatewayv1.Message{
		Kind: &gatewayv1.Message_Stdin{Stdin: &gatewayv1.BytesChunk{Data: data}},
	})
}

// CloseInput half-closes the client→server direction (signals stdin EOF to
// the sandbox process). The output side stays alive until Exit.
func (b *Bridge) CloseInput() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeSendLocked()
}

// closeSendLocked half-closes the client→server direction. The caller MUST hold
// b.mu: grpc-go forbids CloseSend concurrently with SendMsg (they race on the
// stream's unsynchronized half-close flag, and a half-close can land mid-stdin
// frame), and b.mu is what orders it against Feed, which the runner drives from
// its NATS tool_session_input goroutine for the whole life of Run.
//
// The consequence is intended: a Feed blocked in gRPC flow control DELAYS the
// half-close rather than racing it, bounded by the stream context.
func (b *Bridge) closeSendLocked() {
	if b.stream != nil {
		_ = b.stream.CloseSend()
	}
}

// Run dials the gateway, sends Hello, then blocks pumping output until the
// tool exits, the stream ends, or ctx is cancelled. It does NOT return on
// idle — it invokes OnIdle and keeps running so the caller controls teardown
// (typically by deleting the ToolCall CR, which cancels the exec).
func (b *Bridge) Run(ctx context.Context) (BridgeResult, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	opts = append(opts, b.dialOpts...)
	conn, err := grpc.NewClient(b.cfg.GatewayAddr, opts...)
	if err != nil {
		return BridgeResult{ExitCode: -1, ExitReason: "failed"}, err
	}
	defer func() { _ = conn.Close() }()

	stream, err := gatewayv1.NewGatewayClient(conn).Stream(ctx)
	if err != nil {
		return BridgeResult{ExitCode: -1, ExitReason: "failed"}, err
	}
	if b.wrapStream != nil {
		stream = b.wrapStream(stream)
	}
	if err := stream.Send(&gatewayv1.Message{
		Kind: &gatewayv1.Message_Hello{Hello: &gatewayv1.Hello{
			Token:        b.cfg.Token,
			Namespace:    b.cfg.Namespace,
			ToolCallName: b.cfg.ToolCallName,
		}},
	}); err != nil {
		return BridgeResult{ExitCode: -1, ExitReason: "failed"}, err
	}

	b.mu.Lock()
	b.stream = stream
	if b.cfg.IdleTimeout > 0 {
		b.idle = time.AfterFunc(b.cfg.IdleTimeout, b.fireIdle)
	}
	b.mu.Unlock()

	res := BridgeResult{ExitCode: -1, ExitReason: "completed"}
	gotExit := false
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			if gotExit {
				break
			}
			return BridgeResult{ExitCode: -1, ExitReason: "failed"}, rerr
		}
		switch m := msg.Kind.(type) {
		case *gatewayv1.Message_Stdout:
			b.onActivity()
			if b.cfg.OnOutput != nil {
				b.cfg.OnOutput("stdout", m.Stdout.Data)
			}
		case *gatewayv1.Message_Stderr:
			b.onActivity()
			if b.cfg.OnOutput != nil {
				b.cfg.OnOutput("stderr", m.Stderr.Data)
			}
		case *gatewayv1.Message_Exit:
			res.ExitCode = m.Exit.Code
			gotExit = true
			b.mu.Lock()
			// Stop the idle timer in the same critical section that reads
			// b.idled. A timer left armed past a real exit could fire
			// afterwards and retroactively relabel a genuine "completed" run
			// as "idle" — nothing resets it on the Exit frame, only on
			// stdout/stderr.
			b.stopIdleLocked()
			switch {
			case b.idled:
				// Our own watchdog tore this session down, so the exit is
				// ours and not the tool's: OnIdle deletes the ToolCall, whose
				// finalizer cancels the exec, and the gateway faithfully
				// reports that cancellation as a non-empty Exit.Reason with a
				// negative (or signal-derived) code. Reading those fields
				// first would classify every idle teardown as "failed" and
				// make "idle" unreachable. b.idled is the one signal that
				// cannot be missed: fireIdle sets it and releases b.mu
				// strictly before OnIdle can start the teardown that produces
				// this frame.
				res.ExitReason = "idle"
			case m.Exit.Reason != "" || m.Exit.Code != 0:
				res.ExitReason = "failed"
			}
			// Half-close the client→server direction so the gateway's
			// stdin reader goroutine unblocks (its stream.Recv returns
			// io.EOF). Without this, the server's 3-error collector waits
			// forever for the stdin goroutine to finish, the gRPC stream
			// never terminates, and our next stream.Recv blocks until ctx
			// cancellation — manifesting as a multi-second hang between
			// the tool's logical exit and the OnTerminal callback that
			// surfaces the terminal delta to the channel.
			b.closeSendLocked()
			b.mu.Unlock()
		}
	}
	b.mu.Lock()
	b.stopIdleLocked()
	// The Exit arm already decided the reason authoritatively, so only the
	// no-Exit-frame path (the stream ended on the teardown before the gateway
	// could report one) is left to classify here.
	if !gotExit && b.idled && res.ExitReason == "completed" {
		res.ExitReason = "idle"
	}
	b.mu.Unlock()
	return res, nil
}

func (b *Bridge) onActivity() {
	b.mu.Lock()
	b.resetIdleLocked()
	b.mu.Unlock()
}

func (b *Bridge) resetIdleLocked() {
	if b.idle != nil {
		b.idle.Reset(b.cfg.IdleTimeout)
	}
}

func (b *Bridge) stopIdleLocked() {
	if b.idle != nil {
		b.idle.Stop()
	}
}

func (b *Bridge) fireIdle() {
	b.mu.Lock()
	b.idled = true
	cb := b.cfg.OnIdle
	b.mu.Unlock()
	if cb != nil {
		cb()
	}
}
