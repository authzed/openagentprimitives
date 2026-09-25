package sandbox_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	streamRegistry "github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
	gatewayv1 "github.com/authzed/openagentprimitives/pkg/web/gateway/v1"
)

// echoGatewayServer is a minimal gateway: reads Hello, echoes Stdin back
// as Stdout, sends Exit on close-send. Lives in a bufconn so tests don't
// open real sockets. It records the raw token presented in the Hello so
// tests can assert the bridge sent the runner's in-memory preimage.
type echoGatewayServer struct {
	gatewayv1.UnimplementedGatewayServer
	mu         sync.Mutex
	helloToken string
}

func (s *echoGatewayServer) lastHelloToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.helloToken
}

func (s *echoGatewayServer) Stream(stream gatewayv1.Gateway_StreamServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return nil
	}
	s.mu.Lock()
	s.helloToken = hello.Token
	s.mu.Unlock()
	for {
		msg, err := stream.Recv()
		if err != nil {
			break
		}
		if c := msg.GetStdin(); c != nil {
			_ = stream.Send(&gatewayv1.Message{
				Kind: &gatewayv1.Message_Stdout{Stdout: &gatewayv1.BytesChunk{Data: c.Data}},
			})
		}
	}
	_ = stream.Send(&gatewayv1.Message{
		Kind: &gatewayv1.Message_Exit{Exit: &gatewayv1.Exit{Code: 0}},
	})
	return nil
}

func startEchoGateway(t *testing.T) (echo *echoGatewayServer, dialer func(context.Context, string) (net.Conn, error), stop func()) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	echo = &echoGatewayServer{}
	gatewayv1.RegisterGatewayServer(gsrv, echo)
	go func() { _ = gsrv.Serve(lis) }()
	return echo,
		func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		},
		func() {
			gsrv.Stop()
			_ = lis.Close()
		}
}

func newInteractiveFakeClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&spiceboxv1alpha1.ToolCall{}).
		Build()
}

// TestGenerateStreamTokenAndHash asserts the runner-side stream-token
// commitment: the raw token decodes to 32 bytes of entropy, the hash is the
// hex-encoded SHA-256 of the raw token, and successive calls produce distinct
// tokens.
func TestGenerateStreamTokenAndHash(t *testing.T) {
	token, hash, err := sandbox.GenerateStreamTokenAndHash()
	require.NoError(t, err, "generate must succeed")

	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err, "token is base64url")
	assert.Len(t, raw, 32, "token carries 32 bytes of entropy")

	sum := sha256.Sum256([]byte(token))
	assert.Equal(t, hex.EncodeToString(sum[:]), hash,
		"hash is hex(sha256(token)) — the commitment the operator and gateway see")
	assert.Len(t, hash, 64, "hex-encoded SHA-256 is 64 chars")
	assert.NotEqual(t, token, hash, "the committed hash is never the raw token")

	token2, hash2, err := sandbox.GenerateStreamTokenAndHash()
	require.NoError(t, err)
	assert.NotEqual(t, token, token2, "each call mints a fresh token")
	assert.NotEqual(t, hash, hash2, "fresh token → fresh hash")
}

func TestSandboxInteractive_CreatesInteractiveToolCallAndBridges(t *testing.T) {
	c := newInteractiveFakeClient(t)

	// Background watcher: as soon as the ToolCall appears, stamp
	// status.streaming so executeInteractive's pollStreamingEndpoint sees it.
	go func() {
		key := client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}
		for {
			var tc spiceboxv1alpha1.ToolCall
			if err := c.Get(context.Background(), key, &tc); err == nil {
				tc.Status.Streaming = &spiceboxv1alpha1.StreamingEndpoint{
					Available:       true,
					GatewayEndpoint: "passthrough:///bufnet",
				}
				if err := c.Status().Update(context.Background(), &tc); err == nil {
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	echo, dialer, stop := startEchoGateway(t)
	defer stop()

	reg := operations.New(nil, nil)
	op := reg.Begin("interactive test op")

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "alice",
		AgentSessionUID: "uid-1",
		K8sClient:       c,
		BundleSessions:  map[string]string{"code": "alice-code"},
		Operations:      reg,
	}

	var outMu sync.Mutex
	var outChunks [][]byte
	var terminal struct {
		ref    string
		reason string
		code   int32
		called bool
	}

	hooks := sandbox.InteractiveHooks{
		OnOutput: func(ref, stream string, data []byte) {
			outMu.Lock()
			outChunks = append(outChunks, append([]byte(nil), data...))
			outMu.Unlock()
		},
		OnTerminal: func(ref, reason string, code int32) {
			outMu.Lock()
			terminal.ref = ref
			terminal.reason = reason
			terminal.code = code
			terminal.called = true
			outMu.Unlock()
		},
		Register: func(ref string, feed func([]byte) error) func() {
			// Feed once after registration to drive the bridge round-trip.
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = feed([]byte("hello\n"))
			}()
			return func() {}
		},
		IdleTimeoutDefault: time.Minute,
		BridgeDialOpts:     []grpc.DialOption{grpc.WithContextDialer(dialer)},
	}

	ctx, cancel := context.WithTimeout(sandbox.WithInteractiveHooks(context.Background(), hooks), 10*time.Second)
	defer cancel()
	ctx = sandbox.WithIDs(ctx, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu1"})

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "claude",
		Description:  "interactive claude code",
		ToolspecName: "claude-code",
		PollInterval: 5 * time.Millisecond,
		Subcommand:   &toolkit.Subcommand{Mode: toolkit.SubcommandModeInteractive},
	})

	argsJSON, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "drive an interactive session",
		"args":         []string{},
	})
	require.NoError(t, err)

	// Drive the close-send so the echo server sends Exit and the bridge returns.
	go func() {
		// Wait for the bridge to be up + the feed to flush, then trigger end.
		time.Sleep(200 * time.Millisecond)
		// Deleting the ToolCall would trigger the controller's deletion path
		// to cancel the exec, but here we're not running the controller — so
		// we cancel the outer ctx instead, which closes the bridge's recv loop.
		// Bridge will return with whatever exit message it received first.
		// Since echoGateway sends Exit on its own when the client closes
		// CloseSend (which the bridge doesn't do here), we just let it run
		// to ctx cancellation; the bridge handles Recv errors by returning.
	}()

	res, runErr := stool.Execute(ctx, json.RawMessage(argsJSON), sess)
	require.NoError(t, runErr)

	// The ToolCall was created with Mode: interactive.
	var tc spiceboxv1alpha1.ToolCall
	require.NoError(t,
		c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}, &tc))
	assert.Equal(t, spiceboxv1alpha1.ToolCallModeInteractive, tc.Spec.Mode)
	assert.Equal(t, "alice-code", tc.Spec.Session)
	assert.Equal(t, "claude", tc.Spec.Tool)
	assert.Equal(t, time.Minute, tc.Spec.IdleTimeout.Duration, "idle default applied")
	require.Len(t, tc.OwnerReferences, 1)
	assert.Equal(t, "uid-1", string(tc.OwnerReferences[0].UID))

	// The runner commits the stream token by hash: spec.streamTokenHash is a
	// 64-char hex SHA-256, and the raw token NEVER appears in spec or status.
	assert.NotEmpty(t, tc.Spec.StreamTokenHash, "spec.streamTokenHash is committed at creation")
	assert.Len(t, tc.Spec.StreamTokenHash, 64, "streamTokenHash is hex(sha256) — 64 hex chars")
	require.NotNil(t, tc.Status.Streaming, "status.streaming populated")
	assert.Equal(t, "passthrough:///bufnet", tc.Status.Streaming.GatewayEndpoint)

	// The bridge presented the RAW token over the wire. Its SHA-256 must equal
	// the hash committed to the spec — proving the runner held the only copy
	// of the preimage and handed it straight to the gateway client.
	presented := echo.lastHelloToken()
	require.NotEmpty(t, presented, "bridge must have presented a token in Hello")
	sum := sha256.Sum256([]byte(presented))
	assert.Equal(t, hex.EncodeToString(sum[:]), tc.Spec.StreamTokenHash,
		"sha256(presented raw token) == spec.streamTokenHash")
	assert.NotEqual(t, presented, tc.Spec.StreamTokenHash,
		"the raw token presented is the preimage, not the committed hash")

	// OnOutput received the echoed bytes.
	outMu.Lock()
	chunks := outChunks
	term := terminal
	outMu.Unlock()
	require.NotEmpty(t, chunks, "expected at least one OnOutput chunk")
	assert.Equal(t, []byte("hello\n"), chunks[0])

	// OnTerminal fired with this ToolCall's ref.
	assert.True(t, term.called, "OnTerminal should fire")
	assert.Equal(t, "alice-1-tu1", term.ref)
	// tool.Result.Terminal=true on this path because the bridge exits via
	// ctx cancellation (10s timeout) before the echo server can send a
	// clean Exit — the bridge returns through the runErr branch in
	// executeInteractive, which carries Terminal=true to signal that the
	// runner should fail-fast rather than re-dispatch.
	assert.True(t, res.Terminal, "tool.Result.Terminal=true on runErr (ctx cancel) return")

	// Operation registry recorded the call.
	gotOp, ok := reg.Get(op.ID)
	require.True(t, ok, "operation should exist")
	require.NotEmpty(t, gotOp.Calls, "operation registry should record the interactive call")
	assert.Equal(t, "code_claude", gotOp.Calls[0].Tool)
}

// stubStreamFactory mints a parser that wraps each stdout chunk in a
// text_delta event. Registered under a unique kind so it doesn't
// collide with claude-stream-json's blank-import registration.
type stubStreamFactory struct{ kind string }

func (s *stubStreamFactory) Kind() string              { return s.kind }
func (s *stubStreamFactory) New() toolkitstream.Parser { return &stubParser{} }

type stubParser struct{}

func (*stubParser) Parse(chunk []byte) ([]toolkitstream.Event, error) {
	return []toolkitstream.Event{{Type: toolkitstream.EventTextDelta, Text: string(chunk)}}, nil
}
func (*stubParser) Done() ([]toolkitstream.Event, error) { return nil, nil }

func (*stubParser) Outcome() toolkitstream.Outcome { return toolkitstream.Outcome{} }

func TestExecuteInteractive_RoutesStdoutThroughParserWhenToolkitDeclaresStreamFormat(t *testing.T) {
	// Reset the registry on exit so the registered stub doesn't leak
	// to subsequent tests (e.g. claude-stream-json factory init wiring).
	t.Cleanup(streamRegistry.Reset)
	streamRegistry.Register(&stubStreamFactory{kind: "stub-format"})

	c := newInteractiveFakeClient(t)

	go func() {
		key := client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}
		for {
			var tc spiceboxv1alpha1.ToolCall
			if err := c.Get(context.Background(), key, &tc); err == nil {
				tc.Status.Streaming = &spiceboxv1alpha1.StreamingEndpoint{
					Available:       true,
					GatewayEndpoint: "passthrough:///bufnet",
				}
				if err := c.Status().Update(context.Background(), &tc); err == nil {
					return
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	_, dialer, stop := startEchoGateway(t)
	defer stop()

	reg := operations.New(nil, nil)
	op := reg.Begin("stream-parser test op")

	sess := &tool.SessionContext{
		Namespace:       "default",
		Name:            "alice",
		AgentSessionUID: "uid-1",
		K8sClient:       c,
		BundleSessions:  map[string]string{"code": "alice-code"},
		Operations:      reg,
	}

	var (
		mu        sync.Mutex
		gotEvents []toolkitstream.Event
		rawCalls  int
	)
	hooks := sandbox.InteractiveHooks{
		OnOutput: func(_, _ string, _ []byte) {
			mu.Lock()
			defer mu.Unlock()
			rawCalls++
		},
		OnEvent: func(_, _, _ string, ev toolkitstream.Event) {
			mu.Lock()
			defer mu.Unlock()
			gotEvents = append(gotEvents, ev)
		},
		OnTerminal: func(_, _ string, _ int32) {},
		Register: func(_ string, feed func([]byte) error) func() {
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = feed([]byte("hello\n"))
			}()
			return func() {}
		},
		IdleTimeoutDefault: time.Minute,
		BridgeDialOpts:     []grpc.DialOption{grpc.WithContextDialer(dialer)},
	}

	ctx, cancel := context.WithTimeout(sandbox.WithInteractiveHooks(context.Background(), hooks), 10*time.Second)
	defer cancel()
	ctx = sandbox.WithIDs(ctx, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu1"})

	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "claude",
		Description:  "interactive claude code",
		ToolspecName: "claude-code",
		PollInterval: 5 * time.Millisecond,
		Subcommand:   &toolkit.Subcommand{Mode: toolkit.SubcommandModeInteractive},
		Toolkit:      &toolkit.Toolkit{StreamFormat: "stub-format"},
	})

	argsJSON, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "drive an interactive session with a parser",
		"args":         []string{},
	})
	require.NoError(t, err)

	_, runErr := stool.Execute(ctx, json.RawMessage(argsJSON), sess)
	require.NoError(t, runErr)

	// Register's fixture feeds on its own goroutine after a 50ms sleep, so the
	// event is not guaranteed to have arrived when Execute returns — the test
	// must wait for the effect it asserts rather than assume Execute outlasted
	// the sleep plus the parse pipeline. It usually does; under a loaded suite
	// it does not, and the assertion below then indexes an empty slice and
	// panics, taking the rest of the package's tests with it.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gotEvents) > 0
	}, 5*time.Second, 10*time.Millisecond,
		"OnEvent should fire when a parser is registered")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 0, rawCalls, "OnOutput must NOT fire for stdout when parser handles it")
	// The echo gateway sends back exactly what we fed; the stub parser
	// wraps each chunk verbatim, so the first event's Text matches the feed.
	assert.Equal(t, toolkitstream.EventTextDelta, gotEvents[0].Type)
	assert.Equal(t, "hello\n", gotEvents[0].Text)
}

// TestExecuteStreaming_StreamModeToolCall verifies the stream-mode dispatch
// path: a subcommand with Mode "stream" creates a Mode=stream ToolCall whose
// deadline is carried by spec.timeout (not idleTimeout/maxDuration), and the
// stdin Feed is NOT registered (stream mode has a closed stdin).
func TestExecuteStreaming_StreamModeToolCall(t *testing.T) {
	cases := []struct {
		name string
		mode string
		// wantMode is the ToolCallMode the dispatch must produce.
		wantMode spiceboxv1alpha1.ToolCallMode
		// wantRegister is whether hooks.Register must be invoked — true for
		// interactive (channel input feeds stdin), false for stream (closed
		// stdin, one-shot).
		wantRegister bool
		// wantTimeout asserts spec.timeout was set (>0) for stream mode.
		wantTimeout bool
		// wantIdle asserts spec.idleTimeout was set (>0) for interactive mode.
		wantIdle bool
	}{
		{
			name:         "stream mode: Mode=stream, spec.timeout set, idleTimeout unset, Register NOT called",
			mode:         "stream",
			wantMode:     spiceboxv1alpha1.ToolCallModeStream,
			wantRegister: false,
			wantTimeout:  true,
			wantIdle:     false,
		},
		{
			name:         "interactive mode: Mode=interactive, idleTimeout set, Register called",
			mode:         "interactive",
			wantMode:     spiceboxv1alpha1.ToolCallModeInteractive,
			wantRegister: true,
			wantTimeout:  false,
			wantIdle:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newInteractiveFakeClient(t)

			// Background watcher: stamp status.streaming once the ToolCall
			// appears so executeStreaming's pollStreamingEndpoint proceeds.
			go func() {
				key := client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}
				for {
					var got spiceboxv1alpha1.ToolCall
					if err := c.Get(context.Background(), key, &got); err == nil {
						got.Status.Streaming = &spiceboxv1alpha1.StreamingEndpoint{
							Available:       true,
							GatewayEndpoint: "passthrough:///bufnet",
						}
						if err := c.Status().Update(context.Background(), &got); err == nil {
							return
						}
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()

			_, dialer, stop := startEchoGateway(t)
			defer stop()

			reg := operations.New(nil, nil)
			op := reg.Begin("streaming dispatch test op")

			sess := &tool.SessionContext{
				Namespace:       "default",
				Name:            "alice",
				AgentSessionUID: "uid-1",
				K8sClient:       c,
				BundleSessions:  map[string]string{"code": "alice-code"},
				Operations:      reg,
			}

			var registerMu sync.Mutex
			registerCalled := false
			hooks := sandbox.InteractiveHooks{
				OnOutput:   func(_, _ string, _ []byte) {},
				OnTerminal: func(_, _ string, _ int32) {},
				Register: func(ref string, feed func([]byte) error) func() {
					registerMu.Lock()
					registerCalled = true
					registerMu.Unlock()
					// Drive the bridge round-trip when registered (interactive).
					go func() {
						time.Sleep(50 * time.Millisecond)
						_ = feed([]byte("hello\n"))
					}()
					return func() {}
				},
				IdleTimeoutDefault: time.Minute,
				BridgeDialOpts:     []grpc.DialOption{grpc.WithContextDialer(dialer)},
			}

			ctx, cancel := context.WithTimeout(sandbox.WithInteractiveHooks(context.Background(), hooks), 10*time.Second)
			defer cancel()
			ctx = sandbox.WithIDs(ctx, sandbox.IDs{TurnIndex: 1, ToolUseID: "tu1"})

			stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
				BundleName:   "code",
				Suffix:       "claude",
				Description:  "streaming claude code",
				ToolspecName: "claude-code",
				PollInterval: 5 * time.Millisecond,
				Subcommand:   &toolkit.Subcommand{Mode: tc.mode},
			})

			argsJSON, err := json.Marshal(map[string]any{
				"operation_id": op.ID,
				"_reason":      "drive a streaming session",
				"args":         []string{},
			})
			require.NoError(t, err)

			// Stream mode never closes the bridge via stdin EOF (no Feed); the
			// echo gateway only sends Exit after the client's CloseSend, which
			// the bridge doesn't issue. Both modes therefore exit via the 10s
			// ctx timeout — fine, the assertions are about the created CR.
			_, runErr := stool.Execute(ctx, json.RawMessage(argsJSON), sess)
			require.NoError(t, runErr)

			var created spiceboxv1alpha1.ToolCall
			require.NoError(t,
				c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "alice-1-tu1"}, &created))

			assert.Equal(t, tc.wantMode, created.Spec.Mode)
			if tc.wantTimeout {
				assert.Greater(t, created.Spec.Timeout.Duration, time.Duration(0), "stream mode sets spec.timeout")
			}
			if tc.wantIdle {
				assert.Greater(t, created.Spec.IdleTimeout.Duration, time.Duration(0), "interactive mode sets idleTimeout")
			} else {
				assert.Zero(t, created.Spec.IdleTimeout.Duration, "stream mode does not set idleTimeout")
			}

			registerMu.Lock()
			gotRegister := registerCalled
			registerMu.Unlock()
			assert.Equal(t, tc.wantRegister, gotRegister,
				"Register is called only for interactive mode (stdin Feed)")
		})
	}
}

func TestSandboxInteractive_MissingHooks(t *testing.T) {
	c := newInteractiveFakeClient(t)
	reg := operations.New(nil, nil)
	op := reg.Begin("no-hooks")
	sess := &tool.SessionContext{
		Namespace: "default", Name: "alice", AgentSessionUID: "uid-1",
		K8sClient: c, BundleSessions: map[string]string{"code": "alice-code"}, Operations: reg,
	}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName: "code", Suffix: "claude", Subcommand: &toolkit.Subcommand{Mode: toolkit.SubcommandModeInteractive},
	})
	argsJSON, _ := json.Marshal(map[string]any{
		"operation_id": op.ID, "_reason": "x", "args": []string{},
	})
	res, err := stool.Execute(context.Background(), json.RawMessage(argsJSON), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError, "should be IsError when hooks missing")
	assert.Contains(t, res.Content, "without InteractiveHooks")
}

func TestComposeStreamResult(t *testing.T) {
	cases := []struct {
		name    string
		res     sandbox.BridgeResult
		outcome *toolkitstream.Outcome
		stdout  []byte
		stderr  []byte
		check   func(t *testing.T, r tool.Result)
	}{
		{
			name:    "parsed success: payload + success status, no IsError, no stderr",
			res:     sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			outcome: &toolkitstream.Outcome{Text: "did the thing", HasResult: true, OK: true, DurationMs: 4321, CostUSD: 0.0123},
			check: func(t *testing.T, r tool.Result) {
				assert.False(t, r.IsError)
				assert.Contains(t, r.Content, "status: success")
				assert.Contains(t, r.Content, "did the thing")
				assert.NotContains(t, r.Content, "stderr")
			},
		},
		{
			name:    "clean exit but tool reported failure: IsError + stderr tail",
			res:     sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			outcome: &toolkitstream.Outcome{Text: "hit the turn limit", HasResult: true, OK: false},
			stderr:  []byte("boom\n"),
			check: func(t *testing.T, r tool.Result) {
				assert.True(t, r.IsError, "HasResult && !OK → IsError even on exit 0")
				assert.Contains(t, r.Content, "status: failed")
				assert.Contains(t, r.Content, "hit the turn limit")
				assert.Contains(t, r.Content, "--- stderr (tail) ---")
				assert.Contains(t, r.Content, "boom")
			},
		},
		{
			name:   "failed exit, no parser: raw stdout tail + stderr tail",
			res:    sandbox.BridgeResult{ExitCode: 1, ExitReason: "failed"},
			stdout: []byte("partial stdout"),
			stderr: []byte("stack trace"),
			check: func(t *testing.T, r tool.Result) {
				assert.True(t, r.IsError)
				assert.Contains(t, r.Content, "status: failed (exit 1)")
				assert.Contains(t, r.Content, "partial stdout")
				assert.Contains(t, r.Content, "stack trace")
			},
		},
		{
			name:   "raw path success: returns stdout tail under success status",
			res:    sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			stdout: []byte("raw tool output"),
			check: func(t *testing.T, r tool.Result) {
				assert.False(t, r.IsError)
				assert.Contains(t, r.Content, "status: success (exit 0)")
				assert.Contains(t, r.Content, "raw tool output")
			},
		},
		{
			name:    "parser present but no terminal result: falls back to exit-code status",
			res:     sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			outcome: &toolkitstream.Outcome{},
			check: func(t *testing.T, r tool.Result) {
				assert.False(t, r.IsError, "no terminal result reported → not an error")
				assert.Contains(t, r.Content, "status: success (exit 0)",
					"no parsed metadata → status line falls back to the exit code")
			},
		},
		{
			name:    "parsed but empty text: falls back to the raw stdout tail",
			res:     sandbox.BridgeResult{ExitCode: 0, ExitReason: "completed"},
			outcome: &toolkitstream.Outcome{HasResult: true, OK: true},
			stdout:  []byte("raw fallback output"),
			check: func(t *testing.T, r tool.Result) {
				assert.False(t, r.IsError)
				assert.Contains(t, r.Content, "raw fallback output",
					"parser yielded no text → orchestrator still gets the tool's stdout")
			},
		},
		{
			name: "idle exit: Terminal + IdleExit set (content unused)",
			res:  sandbox.BridgeResult{ExitCode: -1, ExitReason: "idle"},
			check: func(t *testing.T, r tool.Result) {
				assert.True(t, r.Terminal)
				assert.True(t, r.IdleExit)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sandbox.ComposeStreamResult(tc.res, tc.outcome, tc.stdout, tc.stderr)
			tc.check(t, r)
		})
	}
}

// ensure unused metav1 import doesn't trigger
var _ = metav1.NewTime
