package sandbox

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// scriptedGateway is an in-process gatewayv1.GatewayServer whose terminal frame
// is written by the test rather than by a real process: after the Hello it
// drains the client→server direction and waits on release, then either sends
// the configured Exit frame or ends the stream without one.
//
// It stands in for the production teardown cascade, which no unit test can
// assemble: OnIdle deletes the ToolCall, the ToolCall finalizer cancels the
// exec, and the gateway reports that cancellation as the process's own Exit
// (pkg/web/gateway/server.go:116-126 over pkg/tools/exec/remote/remote.go:274-281).
type scriptedGateway struct {
	gatewayv1.UnimplementedGatewayServer
	// release ends the scripted exec; the test closes it.
	release <-chan struct{}
	// exit is the terminal frame to send. Nil ends the stream with no Exit
	// frame at all, which the client sees as io.EOF.
	exit *gatewayv1.Exit
}

func (s *scriptedGateway) Stream(stream gatewayv1.Gateway_StreamServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetHello() == nil {
		return errors.New("first message must be Hello")
	}

	// Drain the client→server direction so a Feed never blocks on flow
	// control. Ends on the client's half-close (io.EOF) or on RPC teardown.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, rerr := stream.Recv(); rerr != nil {
				return
			}
		}
	}()

	select {
	case <-s.release:
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	if s.exit == nil {
		return nil
	}
	if err := stream.Send(&gatewayv1.Message{Kind: &gatewayv1.Message_Exit{Exit: s.exit}}); err != nil {
		return err
	}
	// Hold the RPC open until the client half-closes, exactly as the real
	// gateway does — it waits on its stdin goroutine, which finishes only when
	// the client's CloseSend turns its Recv into io.EOF.
	select {
	case <-drained:
	case <-stream.Context().Done():
	}
	return nil
}

func startScriptedGateway(t *testing.T, srv *scriptedGateway) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	gatewayv1.RegisterGatewayServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(func() {
		gsrv.Stop()
		_ = lis.Close()
	})
	return func(context.Context, string) (net.Conn, error) { return lis.DialContext(context.Background()) }
}

// newScriptedBridge builds a Bridge wired to a bufconn dialer, optionally
// wrapping the gateway stream. wrap is nil for every test that does not need to
// observe the bridge's own Send/CloseSend ordering.
func newScriptedBridge(
	t *testing.T,
	cfg BridgeConfig,
	dialer func(context.Context, string) (net.Conn, error),
	wrap func(gatewayv1.Gateway_StreamClient) gatewayv1.Gateway_StreamClient,
) *Bridge {
	t.Helper()
	cfg.GatewayAddr = "passthrough:///bufnet"
	cfg.Namespace = "default"
	cfg.ToolCallName = "demo-tool-1"
	cfg.Token = "tok"
	if cfg.OnOutput == nil {
		cfg.OnOutput = func(string, []byte) {}
	}
	b := NewBridge(cfg)
	b.dialOpts = []grpc.DialOption{grpc.WithContextDialer(dialer)}
	b.wrapStream = wrap
	return b
}

// TestBridge_ExitReasonFromRealIdleTimeout drives Run through a real idle
// timeout and asserts the reason it actually produces. The existing coverage of
// the "idle" outcome (authfail_carrier_test.go, interactive_tool_test.go) tests
// composeStreamResult against a hand-constructed BridgeResult{ExitReason:"idle"},
// so nothing asserted that a real run can ever produce that value — which it
// could not: the idle teardown's own cancellation arrives as a non-empty
// Exit.Reason and was stamped "failed" before the idle override was consulted.
func TestBridge_ExitReasonFromRealIdleTimeout(t *testing.T) {
	cases := []struct {
		name          string
		idleTimeout   time.Duration
		releaseOnIdle bool
		exit          *gatewayv1.Exit
		wantReason    string
		wantCode      int32
	}{
		{
			name:          "idle timer fires and the teardown cancels the exec: ExitReason=idle",
			idleTimeout:   100 * time.Millisecond,
			releaseOnIdle: true,
			exit:          &gatewayv1.Exit{Code: -1, Reason: "context canceled"},
			wantReason:    "idle",
			wantCode:      -1,
		},
		{
			name:          "idle timer fires and the stream ends with no Exit frame: ExitReason=idle",
			idleTimeout:   100 * time.Millisecond,
			releaseOnIdle: true,
			exit:          nil,
			wantReason:    "idle",
			wantCode:      -1,
		},
		{
			name:        "tool exits non-zero and the idle timer never fires: ExitReason=failed",
			idleTimeout: time.Hour,
			exit:        &gatewayv1.Exit{Code: 3, Reason: "exit status 3"},
			wantReason:  "failed",
			wantCode:    3,
		},
		{
			name:        "tool exits zero and the idle timer never fires: ExitReason=completed",
			idleTimeout: time.Hour,
			exit:        &gatewayv1.Exit{Code: 0},
			wantReason:  "completed",
			wantCode:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			endExec := func() { releaseOnce.Do(func() { close(release) }) }

			dialer := startScriptedGateway(t, &scriptedGateway{release: release, exit: tc.exit})

			var idleFired atomic.Bool
			br := newScriptedBridge(t, BridgeConfig{
				IdleTimeout: tc.idleTimeout,
				OnIdle: func() {
					idleFired.Store(true)
					// Production OnIdle deletes the ToolCall, whose finalizer
					// cancels the exec; the gateway then reports that
					// cancellation as the process's Exit.
					endExec()
				},
			}, dialer, nil)

			if !tc.releaseOnIdle {
				endExec()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)

			type outcome struct {
				res BridgeResult
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				r, e := br.Run(ctx)
				done <- outcome{r, e}
			}()

			var got outcome
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("bridge.Run did not return")
			}
			require.NoError(t, got.err, "Run must not error on a scripted exit")
			assert.Equal(t, tc.wantReason, got.res.ExitReason)
			assert.Equal(t, tc.wantCode, got.res.ExitCode)
			assert.Equal(t, tc.releaseOnIdle, idleFired.Load(), "OnIdle firing must match the row")
		})
	}
}

// sendCloseDetector wraps a gateway stream to observe whether the bridge ever
// calls CloseSend while one of its own Sends is still in flight — the overlap
// grpc-go's ClientStream contract forbids ("it is not safe to call CloseSend
// concurrently with SendMsg"), and which grpc backs with an unsynchronized
// clientStream.sentLast field.
//
// It reports the violation twice over: `overlap` is a deterministic observation
// that holds with or without -race, and `halfClosed` makes the race detector
// speak. Nothing here releases the parked Send — that is the test's timer's job
// — because a release issued from CloseSend would order the two goroutines and
// hide the race it is meant to expose.
type sendCloseDetector struct {
	gatewayv1.Gateway_StreamClient

	// halfClosed models the unsynchronized half-close flag a ClientStream
	// implementation is entitled to keep, precisely because the contract
	// promises Send and CloseSend are never concurrent. grpc's own clientStream
	// keeps exactly this: sentLast, read in SendMsg and written in CloseSend,
	// neither under cs.mu. Read and written here in the same two places and
	// with the same absent synchronization, so `go test -race` reports the
	// bridge's violation of the contract as the data race it is.
	//
	// grpc's real field is modelled rather than caught directly because it
	// cannot be caught reliably: the transport's own mutexes chain the sending
	// and receiving goroutines into a happens-before order that suppresses the
	// report even when the two calls genuinely overlap. At the interface
	// boundary there is no such chain.
	halfClosed bool

	mu       sync.Mutex
	inFlight bool
	overlap  bool

	// entered is closed once a stdin Send has parked, so the test can trigger
	// the terminal Exit only after the overlap window is genuinely open.
	entered     chan struct{}
	enteredOnce sync.Once
	// park blocks that Send until the test releases it.
	park chan struct{}
}

func (d *sendCloseDetector) sawOverlap() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.overlap
}

func (d *sendCloseDetector) Send(m *gatewayv1.Message) error {
	// The bridge's Hello is sent before the pump starts and can never overlap;
	// only stdin frames are held open.
	if m.GetStdin() == nil {
		return d.Gateway_StreamClient.Send(m)
	}
	d.mu.Lock()
	d.inFlight = true
	d.mu.Unlock()
	d.enteredOnce.Do(func() { close(d.entered) })
	<-d.park

	var err error
	if d.halfClosed {
		// What grpc returns from SendMsg after a CloseSend: the frame is
		// refused outright, so this Feed's stdin never reaches the tool.
		err = errors.New("Send called after CloseSend")
	} else {
		err = d.Gateway_StreamClient.Send(m)
	}

	d.mu.Lock()
	d.inFlight = false
	d.mu.Unlock()
	return err
}

func (d *sendCloseDetector) CloseSend() error {
	d.mu.Lock()
	if d.inFlight {
		d.overlap = true
	}
	d.mu.Unlock()
	if d.halfClosed {
		return nil
	}
	d.halfClosed = true
	return d.Gateway_StreamClient.CloseSend()
}

// TestBridge_CloseSendDoesNotOverlapFeed pins the half-close against the stdin
// writer. Feed holds b.mu across its Send and CloseInput takes b.mu for the
// identical CloseSend, but Run's Exit arm half-closed under no lock at all —
// while the runner's NATS tool_session_input subscription calls Feed from its
// own goroutine for the whole life of Run (interactive_tool.go registers
// br.Feed; internal/cmd/runner/nats.go dispatches to it).
//
// The terminal Exit is triggered by the test goroutine rather than by the
// parked Feed, so that nothing causally links the sending goroutine to the
// receiving one — a chain between them would give the race detector a
// happens-before edge and mask the defect.
func TestBridge_CloseSendDoesNotOverlapFeed(t *testing.T) {
	release := make(chan struct{})
	det := &sendCloseDetector{entered: make(chan struct{}), park: make(chan struct{})}
	dialer := startScriptedGateway(t, &scriptedGateway{release: release, exit: &gatewayv1.Exit{Code: 0}})

	br := newScriptedBridge(t, BridgeConfig{
		IdleTimeout: time.Hour, // irrelevant here; keep the idle timer out of the way
		OnIdle:      func() {},
	}, dialer, func(s gatewayv1.Gateway_StreamClient) gatewayv1.Gateway_StreamClient {
		det.Gateway_StreamClient = s
		return det
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	runDone := make(chan BridgeResult, 1)
	go func() {
		res, _ := br.Run(ctx)
		runDone <- res
	}()

	// Feed only once the stream is published: before that it returns an error
	// and never reaches the wrapped Send.
	require.Eventually(t, func() bool {
		br.mu.Lock()
		defer br.mu.Unlock()
		return br.stream != nil
	}, 5*time.Second, 5*time.Millisecond, "bridge stream must open")

	feedErr := make(chan error, 1)
	go func() { feedErr <- br.Feed([]byte("input")) }()

	select {
	case <-det.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Feed never reached the gateway stream")
	}

	// The Send is parked and the overlap window is open; only a timer closes
	// it, so the bridge cannot influence when it ends. 250ms is three orders of
	// magnitude more than the in-process round trip the unfixed bridge needs to
	// receive the Exit and half-close.
	relTimer := time.AfterFunc(250*time.Millisecond, func() { close(det.park) })
	t.Cleanup(func() { relTimer.Stop() })
	close(release)

	assert.NoError(t, <-feedErr, "the stdin frame must not be refused by an already-issued half-close")

	select {
	case res := <-runDone:
		assert.Equal(t, "completed", res.ExitReason, "a scripted exit 0 is a clean completion")
	case <-time.After(10 * time.Second):
		t.Fatal("bridge.Run did not return")
	}

	assert.False(t, det.sawOverlap(),
		"CloseSend must not run while a Feed's Send is in flight: grpc-go forbids the overlap and races on the stream's half-close flag")
}
