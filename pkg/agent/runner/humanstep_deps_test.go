package runner

// humanstep_deps_test.go is the TDD suite for humanStepRequester.Confirm: a fake
// approvalHost (shared with preferences_deps_test.go) drives the publish/await
// round-trip without a real Loop/orchestrator, proving the exact login_human_step
// wire contract and the approve/deny/timeout outcome mapping.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	browsertool "github.com/authzed/openagentprimitives/pkg/agent/tool/browser"
)

// TestHumanStepRequester_Confirm_PublishesLoginHumanStepAsk pins the wire
// contract: Confirm publishes exactly one ask of kind login_human_step carrying
// the prompt.
func TestHumanStepRequester_Confirm_PublishesLoginHumanStepAsk(t *testing.T) {
	host := &fakeApprovalHost{approved: true, by: "alice-canonical"}
	req := NewHumanStepRequester(host, nil, time.Second)

	_, err := req.Confirm(context.Background(), "approve the push on your phone", nil)
	require.NoError(t, err)

	require.Equal(t, 1, host.publishCall, "Confirm must publish exactly one ask")
	assert.Equal(t, "login_human_step", host.gotAsk.Kind)
	assert.Equal(t, "approve the push on your phone", host.gotAsk.Summary)

	gotJSON, merr := json.Marshal(host.gotAsk.Payload)
	require.NoError(t, merr)
	assert.JSONEq(t, `{"prompt":"approve the push on your phone"}`, string(gotJSON))
}

// TestHumanStepRequester_Confirm_OutcomeMapping is the approve/deny/timeout
// table: each maps to the documented HumanStepOutcome shape — a timeout is
// TimedOut (fail-closed), never a silent success and never miscounted as a deny.
func TestHumanStepRequester_Confirm_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name     string
		approved bool
		by       string
		timedOut bool
		want     browsertool.HumanStepOutcome
	}{
		{
			name:     "approve: Approved with the decider's id",
			approved: true, by: "YWxpY2U",
			want: browsertool.HumanStepOutcome{Approved: true, DecidedBy: "YWxpY2U"},
		},
		{
			name:     "deny: Denied with the decider's id",
			approved: false, by: "Ym9i",
			want: browsertool.HumanStepOutcome{Denied: true, DecidedBy: "Ym9i"},
		},
		{
			name:     "timeout: TimedOut, nobody decided (fail-closed, not a deny)",
			timedOut: true,
			want:     browsertool.HumanStepOutcome{TimedOut: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeApprovalHost{approved: tc.approved, by: tc.by, timedOut: tc.timedOut}
			req := NewHumanStepRequester(host, nil, time.Second)

			got, err := req.Confirm(context.Background(), "approve the push", nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestHumanStepRequester_RequestCode_PublishesLoginHumanStepAsk pins the wire
// contract: RequestCode publishes the SAME login_human_step ask Confirm does (the
// card is one category; the code half just expects free text back).
func TestHumanStepRequester_RequestCode_PublishesLoginHumanStepAsk(t *testing.T) {
	host := &fakeApprovalHost{approved: true, by: "alice-canonical", code: "123456"}
	req := NewHumanStepRequester(host, nil, time.Second)

	_, err := req.RequestCode(context.Background(), "enter the emailed code", nil)
	require.NoError(t, err)

	require.Equal(t, 1, host.publishCall, "RequestCode must publish exactly one ask")
	assert.Equal(t, "login_human_step", host.gotAsk.Kind)
	assert.Equal(t, "enter the emailed code", host.gotAsk.Summary)

	gotJSON, merr := json.Marshal(host.gotAsk.Payload)
	require.NoError(t, merr)
	assert.JSONEq(t, `{"prompt":"enter the emailed code"}`, string(gotJSON))
}

// TestHumanStepRequester_RequestCode_OutcomeMapping is the approve/deny/timeout
// table for the code path. On an approve the typed code is read off the host (and
// take-cleared); a deny or timeout reads no code at all — the field the runner
// would type into stays empty, fail-closed.
func TestHumanStepRequester_RequestCode_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name          string
		approved      bool
		by            string
		timedOut      bool
		hostCode      string
		want          browsertool.HumanStepCodeOutcome
		wantTakeCalls int
	}{
		{
			name:     "approve: Approved with the decider's id and the typed code",
			approved: true, by: "YWxpY2U", hostCode: "123456",
			want:          browsertool.HumanStepCodeOutcome{Approved: true, DecidedBy: "YWxpY2U", Code: "123456"},
			wantTakeCalls: 1,
		},
		{
			name:     "deny: Denied, and NO code is read (nothing to type)",
			approved: false, by: "Ym9i", hostCode: "123456",
			want:          browsertool.HumanStepCodeOutcome{Denied: true, DecidedBy: "Ym9i"},
			wantTakeCalls: 0,
		},
		{
			name:     "timeout: TimedOut, no code read (fail-closed)",
			timedOut: true, hostCode: "123456",
			want:          browsertool.HumanStepCodeOutcome{TimedOut: true},
			wantTakeCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeApprovalHost{approved: tc.approved, by: tc.by, timedOut: tc.timedOut, code: tc.hostCode}
			req := NewHumanStepRequester(host, nil, time.Second)

			got, err := req.RequestCode(context.Background(), "enter the code", nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantTakeCalls, host.takeCodeCall, "the code must be read off the host only on an approval")
		})
	}
}

// TestHumanStepRequester_RequestCode_PublishErrorPropagates proves a publish
// failure surfaces and AwaitDecision is never reached — parity with Confirm.
func TestHumanStepRequester_RequestCode_PublishErrorPropagates(t *testing.T) {
	host := &fakeApprovalHost{publishErr: errors.New("no resolvable approvers")}
	req := NewHumanStepRequester(host, nil, time.Second)

	_, err := req.RequestCode(context.Background(), "enter the code", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no resolvable approvers")
	assert.Equal(t, 0, host.awaitCall, "AwaitDecision must never run after a publish error")
}

// TestHumanStepRequester_Confirm_PublishErrorPropagates proves a publish failure
// (e.g. no resolvable approvers) surfaces rather than being swallowed into a
// zero-value outcome, and AwaitDecision is never reached.
func TestHumanStepRequester_Confirm_PublishErrorPropagates(t *testing.T) {
	host := &fakeApprovalHost{publishErr: errors.New("no resolvable approvers")}
	req := NewHumanStepRequester(host, nil, time.Second)

	_, err := req.Confirm(context.Background(), "approve the push", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no resolvable approvers")
	assert.Equal(t, 0, host.awaitCall, "AwaitDecision must never run after a publish error")
}

// TestHumanStepRequester_Confirm_ZeroTimeout_PrefersHostResolvedTimeout proves
// "one window, one source": a 0 timeout defers to the host's tiered
// resolvedApprovalTimeout (timeoutResolver), not a private constant.
func TestHumanStepRequester_Confirm_ZeroTimeout_PrefersHostResolvedTimeout(t *testing.T) {
	host := &resolvingFakeApprovalHost{
		fakeApprovalHost: fakeApprovalHost{approved: true},
		resolved:         42 * time.Second,
	}
	req := NewHumanStepRequester(host, nil, 0)

	_, err := req.Confirm(context.Background(), "approve the push", nil)
	require.NoError(t, err)
	assert.Equal(t, 42*time.Second, host.gotTimeout)
}

// TestHumanStepRequester_Confirm_ZeroTimeout_FallsBackToDefault proves a host
// that does not resolve a timeout still gets a sane, non-zero wait.
func TestHumanStepRequester_Confirm_ZeroTimeout_FallsBackToDefault(t *testing.T) {
	host := &fakeApprovalHost{approved: true}
	req := NewHumanStepRequester(host, nil, 0)

	_, err := req.Confirm(context.Background(), "approve the push", nil)
	require.NoError(t, err)
	assert.Equal(t, defaultHumanStepTimeout, host.gotTimeout)
}

// TestNewLateBoundHumanStepRequester_LoopNotReady_FailsClosed proves the
// production adapter refuses loudly (never nil-panics) when Confirm or Captcha
// fires before the Loop is stored.
func TestNewLateBoundHumanStepRequester_LoopNotReady_FailsClosed(t *testing.T) {
	req := NewLateBoundHumanStepRequester(func() *Loop { return nil })

	_, err := req.Confirm(context.Background(), "approve the push", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")

	cerr := req.Captcha(context.Background(), "there is a captcha", nil)
	require.Error(t, cerr)
	assert.Contains(t, cerr.Error(), "not ready")
}
