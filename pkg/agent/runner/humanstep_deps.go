package runner

// humanstep_deps.go implements the runner-side browser.HumanStepRequester that a
// browser toolbox's request_human_step tool depends on. It mirrors
// preferences_deps.go exactly — binds ONE approvalHost per Confirm round-trip and
// publishes/awaits through it (the pendingApprovals-lives-on-the-host invariant,
// see hostBinder's doc) — and, like preferencesSaver, it WRITES NOTHING: the
// confirm's authority IS the human click, re-checked server-side by the
// login_human_step category's DecideApprovers policy before the applied envelope
// ever resumes the tool.
//
// Confirm publishes a login_human_step approval and blocks; Captcha publishes a
// one-way notice and does not block (there is no decision leg — the agent has
// reported it cannot proceed).

import (
	"context"
	"fmt"
	"time"

	browsertool "github.com/authzed/openagentprimitives/pkg/agent/tool/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// defaultHumanStepTimeout is the Confirm wait when neither an explicit timeout
// nor a bound host that can resolve the session's tiered approval timeout
// (timeoutResolver) is available. Matches resolvedApprovalTimeout's own fallback
// so an un-resolvable session still waits a sane window rather than 0.
const defaultHumanStepTimeout = 10 * time.Minute

// humanStepRequester implements browser.HumanStepRequester by binding ONE
// approvalHost per Confirm and publishing/awaiting through it. loopFn reaches the
// live Loop for the non-blocking Captcha notice publish.
type humanStepRequester struct {
	binder  hostBinder
	loopFn  func() *Loop
	timeout time.Duration // resolved via the bound host's timeoutResolver, else defaultHumanStepTimeout, when <= 0
}

var _ browsertool.HumanStepRequester = (*humanStepRequester)(nil)

// NewLateBoundHumanStepRequester is the production adapter: a humanStepRequester
// over a lateBoundHost, so internal/cmd/runner can wire it into
// browser.Synthesize before the session's Loop exists. loopFn is the same
// atomic-pointer accessor main.go threads through PreferenceSaver / LeakageGate /
// etc. The bound *runnerHost's own ResolveTimeout supplies the tiered approval
// window (timeout 0 here).
func NewLateBoundHumanStepRequester(loopFn func() *Loop) browsertool.HumanStepRequester {
	return &humanStepRequester{binder: &lateBoundHost{loopFn: loopFn}, loopFn: loopFn}
}

// NewHumanStepRequester constructs the adapter over an already-live host and Loop
// — the test seam (any future caller holding a live *runnerHost). timeout <= 0
// defers to the host's timeoutResolver (if any) or defaultHumanStepTimeout.
func NewHumanStepRequester(host approvalHost, loop *Loop, timeout time.Duration) browsertool.HumanStepRequester {
	return &humanStepRequester{binder: staticHost{h: host}, loopFn: func() *Loop { return loop }, timeout: timeout}
}

// wireScreenshot attaches cap to host's captureScreenshot field, best-effort:
// if host doesn't happen to be a *runnerHost (a test fake need not implement
// it) or s.loopFn resolves no live Loop, it silently does nothing and the
// login_human_step card simply carries no screenshot — never an error, since
// nothing here can fail the round-trip.
//
// This is the SAME capturer wiring executeToolContained does for the
// web_host#act acting-approval (D-R3: request_human_step is itself a browser
// tool, so both call sites share captureBrowserScreenshotFn — one Loop
// method, two *runnerHost values, because hostBinder deliberately binds a
// FRESH host per round-trip (see its doc) rather than reusing
// executeToolContained's).
func (s *humanStepRequester) wireScreenshot(host approvalHost, cap browsertool.BrowserScreenshotCapturer) {
	if cap == nil {
		return
	}
	rh, ok := host.(*runnerHost)
	if !ok {
		return
	}
	l := s.loopFn()
	if l == nil {
		return
	}
	rh.captureScreenshot = l.captureBrowserScreenshotFn(
		channelevents.SessionRef{Namespace: rh.sess.Namespace, Name: rh.sess.Name}, cap)
}

// Confirm implements browser.HumanStepRequester: bind the ONE host this
// round-trip runs against, publish the login_human_step card, then block for the
// decision ON THAT SAME HOST. It writes nothing — the approved bool alone tells
// request_human_step whether the person completed the step. A timeout surfaces as
// TimedOut (fail-closed: never a silent success).
func (s *humanStepRequester) Confirm(ctx context.Context, prompt string, capturer browsertool.BrowserScreenshotCapturer) (browsertool.HumanStepOutcome, error) {
	if s.binder == nil {
		return browsertool.HumanStepOutcome{}, fmt.Errorf("human step unavailable (session not ready)")
	}
	host, err := s.binder.BindHost()
	if err != nil {
		return browsertool.HumanStepOutcome{}, err
	}
	s.wireScreenshot(host, capturer)

	reqID, err := host.PublishApproval(ctx, pipeline.ApprovalAsk{
		Kind:    "login_human_step",
		Summary: prompt,
		Payload: map[string]any{"prompt": prompt},
	})
	if err != nil {
		return browsertool.HumanStepOutcome{}, err
	}

	timeout := s.timeout
	if timeout <= 0 {
		if tr, ok := host.(timeoutResolver); ok {
			timeout = tr.ResolveTimeout()
		} else {
			timeout = defaultHumanStepTimeout
		}
	}

	approved, by, timedOut, err := host.AwaitDecision(ctx, reqID, timeout)
	if err != nil {
		return browsertool.HumanStepOutcome{}, err
	}
	return browsertool.HumanStepOutcome{
		Approved:  approved,
		Denied:    !approved && !timedOut,
		TimedOut:  timedOut,
		DecidedBy: by,
	}, nil
}

// humanStepCodeHost is the OPTIONAL capability the bound approvalHost satisfies to
// return the human's typed login code captured during the login_human_step
// code round-trip. Mirrors timeoutResolver's optional-capability shape: *runnerHost
// implements it (takeHumanStepCode); a test fake implements it only when the
// scenario drives a code. The confirm path never reads it.
type humanStepCodeHost interface {
	takeHumanStepCode() string
}

// RequestCode implements browser.HumanStepRequester: publish the login_human_step
// card (identical to Confirm's — the card is the same category; the code half just
// expects a free-text answer back), block for the decision ON THE SAME HOST, then
// read the human's typed code off that host. Writes nothing; the caller types the
// code into the page. A timeout is fail-closed (TimedOut, no code).
func (s *humanStepRequester) RequestCode(ctx context.Context, prompt string, capturer browsertool.BrowserScreenshotCapturer) (browsertool.HumanStepCodeOutcome, error) {
	if s.binder == nil {
		return browsertool.HumanStepCodeOutcome{}, fmt.Errorf("human step unavailable (session not ready)")
	}
	host, err := s.binder.BindHost()
	if err != nil {
		return browsertool.HumanStepCodeOutcome{}, err
	}
	s.wireScreenshot(host, capturer)

	reqID, err := host.PublishApproval(ctx, pipeline.ApprovalAsk{
		Kind:    "login_human_step",
		Summary: prompt,
		Payload: map[string]any{"prompt": prompt},
	})
	if err != nil {
		return browsertool.HumanStepCodeOutcome{}, err
	}

	timeout := s.timeout
	if timeout <= 0 {
		if tr, ok := host.(timeoutResolver); ok {
			timeout = tr.ResolveTimeout()
		} else {
			timeout = defaultHumanStepTimeout
		}
	}

	approved, by, timedOut, err := host.AwaitDecision(ctx, reqID, timeout)
	if err != nil {
		return browsertool.HumanStepCodeOutcome{}, err
	}

	// The code rides back out-of-band on the SAME host (one host per round-trip —
	// see hostBinder), captured by AwaitDecision's login_human_step case. Read it
	// ONLY on an approval; take-and-clear so the secret does not linger.
	var code string
	if approved {
		if cr, ok := host.(humanStepCodeHost); ok {
			code = cr.takeHumanStepCode()
		}
	}

	return browsertool.HumanStepCodeOutcome{
		Approved:  approved,
		Denied:    !approved && !timedOut,
		TimedOut:  timedOut,
		DecidedBy: by,
		Code:      code,
	}, nil
}

// Captcha implements browser.HumanStepRequester: surface the one-way
// login_captcha notice on the live Loop. It does not block and writes nothing.
// The capturer (the live browser session) is threaded into the notice
// publisher so the notice carries a redacted screenshot — the surface where a
// screenshot matters MOST, since a person must see the page to take over.
func (s *humanStepRequester) Captcha(ctx context.Context, prompt string, capturer browsertool.BrowserScreenshotCapturer) error {
	if s.loopFn == nil {
		return fmt.Errorf("human step captcha unavailable (session not ready)")
	}
	l := s.loopFn()
	if l == nil {
		return fmt.Errorf("human step captcha unavailable (session not ready)")
	}
	return l.publishLoginCaptchaNotice(ctx, prompt, capturer)
}
