package runner

// browser_screenshot_wiring_test.go is the TDD suite for Task 4's D-R3 wiring:
// capture + store + attach a redacted screenshot when a BROWSER interaction is
// published, best-effort and fail-closed, and NEVER for a non-browser
// interaction. It exercises both seams the wiring touches:
//
//   - The HOST-BUILDER seam: buildPlanGatePending (the web_host#act acting
//     approval) and buildLoginHumanStepPending (request_human_step's
//     login_human_step ask) attach payload.Screenshot from h.captureScreenshot
//     when it is set and returns a non-nil ref, and leave it nil otherwise.
//   - The DISPATCH seam: executeToolContained sets host.captureScreenshot ONLY
//     when the dispatched tool.Tool implements browser.ScreenshotProvider with
//     a non-nil capturer (D-R3) — proven end to end through the real capture +
//     store path (storeScreenshotArtifact over a fake k8s client), a capture
//     error (Screenshot stays nil, the ask still publishes, and the failure is
//     logged), and a plain tool that never captures at all.
//
// Required assertions (brief + D-R3 ruling):
//
//	(i)   a browser ask with a working capturer sets payload.Screenshot
//	(ii)  a capture error leaves Screenshot nil AND the interaction still
//	      publishes (never blocks the approval), with the failure logged
//	(iii) a non-browser tool never invokes any capturer at all

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	browsertool "github.com/authzed/openagentprimitives/pkg/agent/tool/browser"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// fakeCapturer is a stub browser.BrowserScreenshotCapturer: returns a scripted
// PNG or error and counts calls, so a test can prove it was (or was never)
// invoked.
type fakeCapturer struct {
	mu    sync.Mutex
	calls int
	png   []byte
	err   error
}

func (f *fakeCapturer) CaptureRedacted(_ context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.png, nil
}

func (f *fakeCapturer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---------------------------------------------------------------------------
// Host-builder seam: buildPlanGatePending / buildLoginHumanStepPending
// ---------------------------------------------------------------------------

// TestBuildPlanGatePending_AttachesScreenshot_WhenCaptureSucceeds proves the
// web_host#act acting-approval card carries the screenshot the host's
// captureScreenshot closure returns.
func TestBuildPlanGatePending_AttachesScreenshot_WhenCaptureSucceeds(t *testing.T) {
	var published channelevents.Envelope
	h := planGateHost(t, &published)
	ref := &channelevents.AttachmentRef{RenderName: "shot-1", ArtifactID: "art-1", MIME: "image/png", Filename: "page.png", AltText: "current page"}
	var calls int
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef {
		calls++
		return ref
	}

	reqID, err := h.PublishApproval(context.Background(), planPhaseAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	assert.Equal(t, 1, calls, "the plan_phase builder must call captureScreenshot exactly once")
	payload := decodeInteractionRequest(t, published)
	require.NotNil(t, payload.Screenshot, "a working capturer must attach a screenshot")
	assert.Equal(t, "shot-1", payload.Screenshot.RenderName)
	assert.Equal(t, "art-1", payload.Screenshot.ArtifactID)
}

// TestBuildPlanGatePending_NoScreenshot_WhenCaptureReturnsNil proves a capture
// that resolved to nil (the real closure already logged why) leaves
// Screenshot unset — and the card still publishes.
func TestBuildPlanGatePending_NoScreenshot_WhenCaptureReturnsNil(t *testing.T) {
	var published channelevents.Envelope
	h := planGateHost(t, &published)
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef { return nil }

	reqID, err := h.PublishApproval(context.Background(), planPhaseAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()), "a nil screenshot must never block the publish")

	payload := decodeInteractionRequest(t, published)
	assert.Nil(t, payload.Screenshot)
}

// TestBuildPlanGatePending_NoScreenshot_WhenCaptureScreenshotUnset proves the
// browser-only gate at the builder: a host built for a non-browser call never
// has captureScreenshot set (executeToolContained leaves it nil), so the
// builder attempts no capture at all.
func TestBuildPlanGatePending_NoScreenshot_WhenCaptureScreenshotUnset(t *testing.T) {
	var published channelevents.Envelope
	h := planGateHost(t, &published) // h.captureScreenshot left nil, as for any non-browser call

	reqID, err := h.PublishApproval(context.Background(), planPhaseAsk())
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	payload := decodeInteractionRequest(t, published)
	assert.Nil(t, payload.Screenshot)
}

// TestBuildLoginHumanStepPending_AttachesScreenshot_WhenCaptureSucceeds mirrors
// the plan_phase case for request_human_step's login_human_step ask — the
// OTHER browser-sourced publish site D-R3 unifies via captureBrowserScreenshotFn.
func TestBuildLoginHumanStepPending_AttachesScreenshot_WhenCaptureSucceeds(t *testing.T) {
	var published channelevents.Envelope
	l := loginHumanStepLoop(approval.New(), func(_, _ string, env channelevents.Envelope) { published = env })
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	ref := &channelevents.AttachmentRef{RenderName: "shot-2", ArtifactID: "art-2", MIME: "image/png", Filename: "page.png", AltText: "current page"}
	h.captureScreenshot = func(_ context.Context) *channelevents.AttachmentRef { return ref }

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("approve the push"))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	payload := decodeInteractionRequest(t, published)
	require.NotNil(t, payload.Screenshot, "a working capturer must attach a screenshot to the login_human_step card")
	assert.Equal(t, "shot-2", payload.Screenshot.RenderName)
}

// TestBuildLoginHumanStepPending_NoScreenshot_WhenCaptureScreenshotUnset proves
// the same browser-only gate for the login_human_step ask.
func TestBuildLoginHumanStepPending_NoScreenshot_WhenCaptureScreenshotUnset(t *testing.T) {
	var published channelevents.Envelope
	l := loginHumanStepLoop(approval.New(), func(_, _ string, env channelevents.Envelope) { published = env })
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), loginHumanStepAsk("approve the push"))
	require.NoError(t, err)
	pa, ok := h.pending().m[reqID]
	require.True(t, ok)
	require.NoError(t, pa.onPublish(context.Background()))

	payload := decodeInteractionRequest(t, published)
	assert.Nil(t, payload.Screenshot)
}

// ---------------------------------------------------------------------------
// Dispatch seam: executeToolContained's browser.ScreenshotProvider wiring
// ---------------------------------------------------------------------------

// fakeBrowserDispatchTool is a fakeDispatchTool that ALSO implements
// browser.ScreenshotProvider, standing in for a real *browserTool (whose
// ScreenshotCapturer() returns its live *Session) without pulling in chromedp.
type fakeBrowserDispatchTool struct {
	fakeDispatchTool
	capturer browsertool.BrowserScreenshotCapturer
}

func (f *fakeBrowserDispatchTool) ScreenshotCapturer() browsertool.BrowserScreenshotCapturer {
	return f.capturer
}

var _ browsertool.ScreenshotProvider = (*fakeBrowserDispatchTool)(nil)

// screenshotAskHook publishes a fixed plan_phase ApprovalAsk at PreToolCall —
// the same host.PublishApproval → buildPlanGatePending machinery the real
// plan-gate hook drives, without reproducing its ceiling/slot logic, since
// this suite only cares whether the published payload's Screenshot field
// reflects what executeToolContained wired onto the host. The ask's own
// Timeout is set short so AwaitDecision resolves (as a fail-closed timeout)
// quickly — the test only inspects what was PUBLISHED, not the outcome.
type screenshotAskHook struct{}

func (screenshotAskHook) Name() string             { return "test_screenshot_ask_hook" }
func (screenshotAskHook) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }
func (screenshotAskHook) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	if in.Point != pipeline.PreToolCall {
		return pipeline.Decision{}
	}
	ask := planPhaseAsk()
	ask.Timeout = 20 * time.Millisecond
	return pipeline.Decision{Verdict: pipeline.Allow, Approval: &ask}
}

// screenshotDispatchLoop builds a minimal Loop that can run executeToolContained
// through a plan_phase ask (resolving approvers and publishing), with Client
// and Artifacts wired so the full capture→store→poll-to-Ready→finalize→attach
// path can complete (storeScreenshotArtifact creates an ArtifactRender CR and
// awaitScreenshotReady finalizes it — see browser_screenshot.go). The
// readyOnCreateInterceptor flips the CR straight to Ready on Create, mirroring
// loop_uiresource_test.go's own persistWidget fixture.
func screenshotDispatchLoop(t *testing.T, tools []tool.Tool, published *channelevents.Envelope) *Loop {
	t.Helper()
	l := &Loop{
		Tools:       tools,
		SessionKey:  memory.NamespacedName{Namespace: "ns", Name: "s"},
		Status:      LocalStatusPatcher(),
		Approval:    approval.New(),
		ChannelKind: "slack",
		AgentName:   "demo-agent",
		Client: fake.NewClientBuilder().WithScheme(browserScreenshotTestScheme(t)).
			WithInterceptorFuncs(readyOnCreateInterceptor()).Build(),
		Artifacts: uiResourceTestArtifactsService(),
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@example.com"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, env channelevents.Envelope) error {
			*published = env
			return nil
		},
	}
	reg := pipeline.NewRegistry()
	reg.Register(screenshotAskHook{}, 10)
	loopWithInjectedExecutor(t, l, reg)
	return l
}

// TestExecuteToolContained_AttachesScreenshot_ForScreenshotProviderToolWithWorkingCapturer
// is requirement (i): dispatching a tool that implements browser.ScreenshotProvider
// with a working capturer publishes a plan_phase ask whose payload carries the
// screenshot, through the REAL capture→store→attach path (no stubbed storage).
func TestExecuteToolContained_AttachesScreenshot_ForScreenshotProviderToolWithWorkingCapturer(t *testing.T) {
	cap := &fakeCapturer{png: bytes.Repeat([]byte{0xAB}, 128)} // well under the inline cap
	ft := &fakeBrowserDispatchTool{
		fakeDispatchTool: fakeDispatchTool{name: "browser_click", kind: tool.KindMCP, result: tool.Result{Content: "ok"}},
		capturer:         cap,
	}
	var published channelevents.Envelope
	l := screenshotDispatchLoop(t, []tool.Tool{ft}, &published)
	sess := &tool.SessionContext{Namespace: "ns", Name: "s"}

	_, _ = l.executeToolContained(context.Background(), sess, ft, "browser_click", json.RawMessage(`{}`), "tu-1", "", containParams{})

	assert.Equal(t, 1, cap.callCount(), "the dispatched tool's capturer must be invoked exactly once")
	payload := decodeInteractionRequest(t, published)
	require.NotNil(t, payload.Screenshot, "a ScreenshotProvider tool with a working capturer must attach a screenshot")
	assert.Equal(t, "image/png", payload.Screenshot.MIME)
}

// TestExecuteToolContained_NoScreenshot_WhenCaptureErrors_PublishStillHappens
// is requirement (ii): a capture error leaves Screenshot nil, the ask still
// publishes (an approval must never be blocked by a screenshot failure), and
// the failure is logged.
func TestExecuteToolContained_NoScreenshot_WhenCaptureErrors_PublishStillHappens(t *testing.T) {
	logs := &guardedBuf{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	cap := &fakeCapturer{err: errors.New("cdp: target closed")}
	ft := &fakeBrowserDispatchTool{
		fakeDispatchTool: fakeDispatchTool{name: "browser_click", kind: tool.KindMCP, result: tool.Result{Content: "ok"}},
		capturer:         cap,
	}
	var published channelevents.Envelope
	l := screenshotDispatchLoop(t, []tool.Tool{ft}, &published)
	sess := &tool.SessionContext{Namespace: "ns", Name: "s"}

	_, _ = l.executeToolContained(context.Background(), sess, ft, "browser_click", json.RawMessage(`{}`), "tu-1", "", containParams{})

	assert.Equal(t, 1, cap.callCount(), "the capturer must still be invoked (and fail)")
	payload := decodeInteractionRequest(t, published)
	assert.Nil(t, payload.Screenshot, "a capture failure must never attach a screenshot")
	assert.Contains(t, logs.String(), "browser screenshot: capture failed", "a capture failure must be logged with context")
	assert.Contains(t, logs.String(), "cdp: target closed")
}

// TestExecuteToolContained_NeverCaptures_ForNonScreenshotProviderTool is
// requirement (iii): a plain tool.Tool (no browser.ScreenshotProvider) never
// gets host.captureScreenshot set, so the SAME ask-publishing hook that
// attaches a screenshot for a browser tool attaches nothing here.
func TestExecuteToolContained_NeverCaptures_ForNonScreenshotProviderTool(t *testing.T) {
	ft := &fakeDispatchTool{name: "read_file", kind: tool.KindMCP, result: tool.Result{Content: "ok"}}
	var published channelevents.Envelope
	l := screenshotDispatchLoop(t, []tool.Tool{ft}, &published)
	sess := &tool.SessionContext{Namespace: "ns", Name: "s"}

	_, _ = l.executeToolContained(context.Background(), sess, ft, "read_file", json.RawMessage(`{}`), "tu-1", "", containParams{})

	payload := decodeInteractionRequest(t, published)
	assert.Nil(t, payload.Screenshot, "a non-browser tool must never attach a screenshot")
}

// guardedBuf is a mutex-guarded io.Writer for slog output, mirroring
// failClosedLog (loop_precondition_failclosed_test.go): slog.SetDefault makes
// the handler reachable from any goroutine still running in the package's
// suite, so a bare bytes.Buffer would race under -race.
type guardedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (g *guardedBuf) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *guardedBuf) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}
