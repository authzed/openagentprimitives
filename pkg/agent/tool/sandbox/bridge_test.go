package sandbox

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// fakeGateway is an in-process gatewayv1.GatewayServer for bridge tests.
// It reads the Hello, then echoes any Stdin back as Stdout, and sends Exit
// when the client closes-send or the test cancels its handler context.
type fakeGateway struct {
	gatewayv1.UnimplementedGatewayServer
	// holdExit, if true, never sends Exit — the client decides when to teardown.
	holdExit bool
}

func (s *fakeGateway) Stream(stream gatewayv1.Gateway_StreamServer) error {
	// 1) Hello.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetHello() == nil {
		return errors.New("first message must be Hello")
	}

	// 2) Echo loop: each Stdin from the client becomes a Stdout back.
	done := make(chan struct{})
	var sendErr error
	var sendMu sync.Mutex
	send := func(m *gatewayv1.Message) {
		sendMu.Lock()
		defer sendMu.Unlock()
		if err := stream.Send(m); err != nil && sendErr == nil {
			sendErr = err
		}
	}

	go func() {
		defer close(done)
		for {
			msg, rerr := stream.Recv()
			if errors.Is(rerr, io.EOF) {
				return
			}
			if rerr != nil {
				return
			}
			if c := msg.GetStdin(); c != nil {
				send(&gatewayv1.Message{
					Kind: &gatewayv1.Message_Stdout{
						Stdout: &gatewayv1.BytesChunk{Data: c.Data},
					},
				})
			}
		}
	}()

	// Wait for client to close-send (or handler ctx done).
	select {
	case <-done:
	case <-stream.Context().Done():
	}
	if !s.holdExit {
		send(&gatewayv1.Message{
			Kind: &gatewayv1.Message_Exit{Exit: &gatewayv1.Exit{Code: 0}},
		})
	}
	return sendErr
}

func startFakeGateway(t *testing.T, srv *fakeGateway) (dialer func(context.Context, string) (net.Conn, error), stop func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	gatewayv1.RegisterGatewayServer(gsrv, srv)
	go func() { _ = gsrv.Serve(lis) }()
	return func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		},
		func() {
			gsrv.Stop()
			_ = lis.Close()
		}
}

// newBridgeWithDialer rebuilds the bridge to use a bufconn dialer so tests
// don't open real TCP sockets. Production code path uses grpc.NewClient with
// an address; tests inject a dialer via grpc.WithContextDialer.
func newBridgeWithDialer(cfg BridgeConfig, dialer func(context.Context, string) (net.Conn, error)) *Bridge {
	b := NewBridge(cfg)
	b.dialOpts = []grpc.DialOption{
		grpc.WithContextDialer(dialer),
	}
	return b
}

func TestBridge_PumpsOutputAndAcceptsInput(t *testing.T) {
	dialer, stop := startFakeGateway(t, &fakeGateway{})
	defer stop()

	var mu sync.Mutex
	var got [][]byte
	br := newBridgeWithDialer(BridgeConfig{
		GatewayAddr:  "passthrough:///bufnet",
		Namespace:    "default",
		ToolCallName: "alice-1-x",
		Token:        "tok",
		IdleTimeout:  time.Hour, // disabled for this test
		OnOutput: func(_ string, data []byte) {
			mu.Lock()
			got = append(got, append([]byte(nil), data...))
			mu.Unlock()
		},
		OnIdle: func() {},
	}, dialer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	var res BridgeResult
	var runErr error
	go func() {
		res, runErr = br.Run(ctx)
		close(done)
	}()

	// Wait briefly for the stream to be established, then feed input.
	require.Eventually(t, func() bool {
		return br.Feed([]byte("hello")) == nil
	}, 2*time.Second, 20*time.Millisecond, "Feed should succeed once stream is open")
	br.CloseInput()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge.Run did not return")
	}
	require.NoError(t, runErr)
	assert.Equal(t, "completed", res.ExitReason)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, got, "expected at least one stdout chunk echoed back")
	assert.Equal(t, []byte("hello"), got[0])
}

func TestBridge_IdleTimeoutFiresOnIdle(t *testing.T) {
	dialer, stop := startFakeGateway(t, &fakeGateway{holdExit: true})
	defer stop()

	idle := make(chan struct{}, 1)
	br := newBridgeWithDialer(BridgeConfig{
		GatewayAddr:  "passthrough:///bufnet",
		Namespace:    "default",
		ToolCallName: "alice-1-x",
		Token:        "tok",
		IdleTimeout:  150 * time.Millisecond,
		OnOutput:     func(string, []byte) {},
		OnIdle: func() {
			select {
			case idle <- struct{}{}:
			default:
			}
		},
	}, dialer)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, _ = br.Run(ctx) }()

	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("OnIdle never fired")
	}
}
