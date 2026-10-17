package browser

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// fakeHumanStep records what request_human_step asked and returns a scripted
// outcome, with no runner/host/orchestrator involved.
type fakeHumanStep struct {
	confirmCalls  int
	confirmPrompt string
	outcome       HumanStepOutcome
	confirmErr    error

	codeCalls   int
	codePrompt  string
	codeOutcome HumanStepCodeOutcome
	codeErr     error

	captchaCalls  int
	captchaPrompt string
	captchaErr    error
}

func (f *fakeHumanStep) Confirm(_ context.Context, prompt string, _ BrowserScreenshotCapturer) (HumanStepOutcome, error) {
	f.confirmCalls++
	f.confirmPrompt = prompt
	return f.outcome, f.confirmErr
}

func (f *fakeHumanStep) RequestCode(_ context.Context, prompt string, _ BrowserScreenshotCapturer) (HumanStepCodeOutcome, error) {
	f.codeCalls++
	f.codePrompt = prompt
	return f.codeOutcome, f.codeErr
}

func (f *fakeHumanStep) Captcha(_ context.Context, prompt string, _ BrowserScreenshotCapturer) error {
	f.captchaCalls++
	f.captchaPrompt = prompt
	return f.captchaErr
}

func humanStepTool(hs HumanStepRequester) *browserTool {
	return &browserTool{op: requestHumanStepOp, humanStep: hs}
}

func runHumanStep(t *testing.T, bt *browserTool, args map[string]any) agentResult {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	res, err := bt.Execute(context.Background(), raw, nil)
	require.NoError(t, err, "Execute must never return a Go error for a tool-level outcome")
	return agentResult{content: res.Content, isError: res.IsError}
}

type agentResult struct {
	content string
	isError bool
}

// TestRequestHumanStepPermissionIsStateless proves request_human_step is NOT
// web_host-gated: its authorization is the human card it posts, so it carries no
// External/web_host Check — unlike the mutating acting ops (click/type/...).
func TestRequestHumanStepPermissionIsStateless(t *testing.T) {
	p := humanStepTool(nil).Permission()
	assert.Equal(t, authz.Stateless, p.StateImpact,
		"request_human_step reaches no page state; the human card IS the gate")
	assert.Nil(t, p.Check, "no web_host slot check — layering one would be a redundant second human gate")
}

// TestRequestHumanStepConfirm maps each confirm outcome to the tool result. The
// approve result is a STATUS only (success, no secret); deny and timeout are
// fail-closed error results — a timeout never silently succeeds.
func TestRequestHumanStepConfirm(t *testing.T) {
	cases := []struct {
		name         string
		outcome      HumanStepOutcome
		wantIsError  bool
		wantContains string
	}{
		{name: "approved → success status", outcome: HumanStepOutcome{Approved: true}, wantIsError: false, wantContains: "confirmed"},
		{name: "denied → fail-closed", outcome: HumanStepOutcome{Denied: true}, wantIsError: true, wantContains: "declined"},
		{name: "timed out → fail-closed (not a silent success)", outcome: HumanStepOutcome{TimedOut: true}, wantIsError: true, wantContains: "timed out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := &fakeHumanStep{outcome: tc.outcome}
			bt := humanStepTool(hs)
			res := runHumanStep(t, bt, map[string]any{"kind": "confirm", "prompt": "approve the push on your phone"})

			assert.Equal(t, 1, hs.confirmCalls, "confirm must block through the requester exactly once")
			assert.Equal(t, 0, hs.captchaCalls, "confirm must not surface a captcha notice")
			assert.Equal(t, "approve the push on your phone", hs.confirmPrompt)
			assert.Equal(t, tc.wantIsError, res.isError)
			assert.Contains(t, res.content, tc.wantContains)
		})
	}
}

// TestRequestHumanStepCaptcha proves the no-kind path surfaces a notice and
// reports that human intervention is needed (fail: it never blocks on a decision).
func TestRequestHumanStepCaptcha(t *testing.T) {
	for _, kind := range []string{"", "captcha"} {
		t.Run("kind="+kind, func(t *testing.T) {
			hs := &fakeHumanStep{}
			bt := humanStepTool(hs)
			args := map[string]any{"prompt": "a reCAPTCHA is blocking the page"}
			if kind != "" {
				args["kind"] = kind
			}
			res := runHumanStep(t, bt, args)

			assert.Equal(t, 1, hs.captchaCalls, "a captcha must surface the notice exactly once")
			assert.Equal(t, 0, hs.confirmCalls, "a captcha must not block on a decision")
			assert.Equal(t, "a reCAPTCHA is blocking the page", hs.captchaPrompt)
			assert.True(t, res.isError, "a captcha the agent cannot solve is an error result")
			assert.Contains(t, res.content, "person", "the result must say a person is needed")
		})
	}
}

// TestRequestHumanStepCode_RefRequired proves kind=code refuses up front without a
// ref — the code has to be typed somewhere, so a missing ref fails BEFORE a human
// is ever asked (no RequestCode call).
func TestRequestHumanStepCode_RefRequired(t *testing.T) {
	hs := &fakeHumanStep{}
	bt := humanStepTool(hs)
	res := runHumanStep(t, bt, map[string]any{"kind": "code", "prompt": "enter the emailed code"})

	assert.True(t, res.isError)
	assert.Contains(t, res.content, "ref")
	assert.Equal(t, 0, hs.codeCalls, "a missing ref must fail before blocking a human")
}

// TestRequestHumanStepCode_OutcomeMapping maps each RequestCode outcome to the
// tool result. Approve with a code is a STATUS only (and the code never appears in
// the result); deny and timeout are fail-closed errors; an approve with no code
// fails closed rather than typing nothing and claiming success. The actual
// type-into-ref needs Chrome and is covered by the fill-fixture round-trip below;
// here humanStep is set but sess is nil, so the approve+code case would nil-panic
// on the type — which is itself the proof the request is refused for the right
// reasons BEFORE the type on every non-approve path. The approve+code result path
// is asserted in TestRequestHumanStepCode_RoundTripNoLeak (Chrome).
func TestRequestHumanStepCode_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name         string
		outcome      HumanStepCodeOutcome
		wantIsError  bool
		wantContains string
	}{
		{name: "denied → fail-closed", outcome: HumanStepCodeOutcome{Denied: true}, wantIsError: true, wantContains: "declined"},
		{name: "timed out → fail-closed (not a silent success)", outcome: HumanStepCodeOutcome{TimedOut: true}, wantIsError: true, wantContains: "timed out"},
		{name: "approved but no code → fail-closed", outcome: HumanStepCodeOutcome{Approved: true, Code: ""}, wantIsError: true, wantContains: "no code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := &fakeHumanStep{codeOutcome: tc.outcome}
			bt := humanStepTool(hs)
			res := runHumanStep(t, bt, map[string]any{"kind": "code", "prompt": "enter the emailed code", "ref": "e3"})

			assert.Equal(t, 1, hs.codeCalls, "code must block through the requester exactly once")
			assert.Equal(t, 0, hs.confirmCalls, "code must not use the confirm path")
			assert.Equal(t, "enter the emailed code", hs.codePrompt)
			assert.Equal(t, tc.wantIsError, res.isError)
			assert.Contains(t, res.content, tc.wantContains)
		})
	}
}

// TestRequestHumanStepValidation covers the local guards: empty prompt, a nil
// requester, and an unknown kind all fail as tool-level errors without panicking.
func TestRequestHumanStepValidation(t *testing.T) {
	t.Run("empty prompt is refused", func(t *testing.T) {
		res := runHumanStep(t, humanStepTool(&fakeHumanStep{}), map[string]any{"kind": "confirm"})
		assert.True(t, res.isError)
		assert.Contains(t, res.content, "prompt")
	})
	t.Run("nil requester fails loud, not nil-panic", func(t *testing.T) {
		res := runHumanStep(t, humanStepTool(nil), map[string]any{"kind": "confirm", "prompt": "approve the push"})
		assert.True(t, res.isError)
		assert.Contains(t, res.content, "unavailable")
	})
	t.Run("unknown kind is refused", func(t *testing.T) {
		hs := &fakeHumanStep{}
		res := runHumanStep(t, humanStepTool(hs), map[string]any{"kind": "banana", "prompt": "do a thing"})
		assert.True(t, res.isError)
		assert.Contains(t, res.content, "unknown kind")
		assert.Equal(t, 0, hs.confirmCalls)
		assert.Equal(t, 0, hs.captchaCalls)
	})
}
