package gateway

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// Hello is the ONLY authentication on this stream, and the first Recv is what
// waits for it — so until that Recv returns, the connection is anonymous.
//
// Unbounded, an opened-and-silent stream parked a goroutine, its buffers, and
// an HTTP/2 stream forever. grpc-go's own defaults do not bound it: this server
// is built with no ServerOption, maxConcurrentStreams defaults to MaxUint32,
// and connectionTimeout covers the HTTP/2 handshake only, not the
// application-level wait that follows it. The target is the singleton control
// plane at 256Mi, reachable from any pod carrying a label key its own creator
// supplies.
//
// The failing shape without the deadline is a HANG rather than a wrong answer,
// which is why this asserts a bound and not only a code.
func TestServerStream_SilentClient_IsRefusedAtTheDeadline(t *testing.T) {
	srv := &Server{Registry: NewRegistry(), helloDeadline: 50 * time.Millisecond}

	// A client that dials and then says nothing at all.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	fs := &fakeStreamServer{
		ctx: context.Background(),
		recv: func() (*gatewayv1.Message, error) {
			<-blocked
			return nil, context.Canceled
		},
	}

	done := make(chan error, 1)
	go func() { done <- srv.Stream(fs) }()

	select {
	case err := <-done:
		require.Error(t, err, "a client that never sends Hello must be refused")
		assert.Equal(t, codes.DeadlineExceeded, status.Code(err),
			"the refusal must name the deadline so an operator can tell it from a failed claim")
	case <-time.After(5 * time.Second):
		t.Fatal("Stream never returned: the pre-authentication wait is unbounded")
	}
}

// The deadline must bound only the ANONYMOUS window. Once Hello lands and the
// claim succeeds, an exec stream is legitimately long-lived — an interactive
// tool call runs for minutes — and must not inherit it. A deadline placed in a
// ServerOption instead of here would fail exactly this test.
func TestServerStream_DeadlineDoesNotBoundAnAuthenticatedStream(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	t.Cleanup(func() {
		_ = stdinR.Close()
		_ = stdoutW.Close()
		_ = stderrW.Close()
	})

	// The tool runs until this closes — nothing about the client ends it.
	toolDone := make(chan struct{})
	st := &exec.Stream{
		Stdin: stdinW, Stdout: stdoutR, Stderr: stderrR,
		Wait: func() (exec.Result, error) {
			<-toolDone
			return exec.Result{ExitCode: 0}, nil
		},
		Close: func() error {
			_ = stdoutW.Close()
			_ = stderrW.Close()
			return nil
		},
	}

	reg := NewRegistry()
	token := registerClaimable(t, reg, "tc-long", st, nil)
	srv := &Server{Registry: reg, helloDeadline: 50 * time.Millisecond}

	clientQuiet := make(chan struct{})
	fs := &fakeStreamServer{
		ctx: context.Background(),
		recv: helloThen("default", "tc-long", token, func() (*gatewayv1.Message, error) {
			// The client stays connected and silent AFTER authenticating.
			<-clientQuiet
			return nil, io.EOF
		}),
	}

	done := make(chan error, 1)
	go func() { done <- srv.Stream(fs) }()

	// Well past the hello deadline, the authenticated stream is still served.
	select {
	case err := <-done:
		t.Fatalf("an authenticated stream was torn down by the pre-auth deadline: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// It ends when the TOOL ends, which is the only thing that should end it.
	close(toolDone)
	close(clientQuiet)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after the exec finished")
	}
}
