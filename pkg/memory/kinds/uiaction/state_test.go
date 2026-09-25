package uiaction_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/uiaction"
)

func TestStateFor(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		isError  bool
		timedOut bool
		want     uiaction.State
	}{
		{name: "ok, no tool error -> succeeded", status: channelevents.AppToolCallStatusOK, want: uiaction.StateSucceeded},
		{name: "ok, tool reported its own error -> failed", status: channelevents.AppToolCallStatusOK, isError: true, want: uiaction.StateFailed},
		{name: "denied -> denied", status: channelevents.AppToolCallStatusDenied, want: uiaction.StateDenied},
		{name: "rate limited -> rate_limited", status: channelevents.AppToolCallStatusRateLimited, want: uiaction.StateRateLimited},
		{name: "not found -> failed (a dead button is a failure, not a denial)", status: channelevents.AppToolCallStatusNotFound, want: uiaction.StateFailed},
		{name: "transport error -> failed", status: channelevents.AppToolCallStatusError, want: uiaction.StateFailed},
		{name: "requires_approval -> submitted, and NOT terminal", status: channelevents.AppToolCallStatusRequiresApproval, want: uiaction.StateSubmitted},
		{name: "a lapsed approval deadline is expired, NOT denied", status: channelevents.AppToolCallStatusDenied, timedOut: true, want: uiaction.StateExpired},
		{name: "timedOut outranks even an OK status", status: channelevents.AppToolCallStatusOK, timedOut: true, want: uiaction.StateExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, uiaction.StateFor(tc.status, tc.isError, tc.timedOut))
		})
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []uiaction.State{uiaction.StateSucceeded, uiaction.StateFailed,
		uiaction.StateDenied, uiaction.StateExpired, uiaction.StateRateLimited} {
		assert.True(t, uiaction.IsTerminal(s), "%s must end the lifecycle", s)
	}
	for _, s := range []uiaction.State{uiaction.StateSubmitted, uiaction.StateAwaitingApproval, uiaction.StateRunning} {
		assert.False(t, uiaction.IsTerminal(s), "%s must keep the control disabled", s)
	}
}

func TestDisplayCopyLeaksNothingInternal(t *testing.T) {
	for _, s := range []uiaction.State{uiaction.StateSubmitted, uiaction.StateAwaitingApproval,
		uiaction.StateRunning, uiaction.StateSucceeded, uiaction.StateFailed,
		uiaction.StateDenied, uiaction.StateExpired, uiaction.StateRateLimited} {
		for _, addressed := range []bool{true, false} {
			copyText := uiaction.DisplayCopy(s, addressed)
			assert.NotEmpty(t, copyText, "every state needs copy; a blank caption is a silent state")
			for _, banned := range []string{"ui_action", "AgentUI", "AgentSession", "kubectl",
				"ap.session.", "spicedb", "memory.Scope", "app-tool"} {
				assert.NotContains(t, copyText, banned, "state %q copy must not leak %q", s, banned)
			}
		}
	}
	assert.NotEqual(t,
		uiaction.DisplayCopy(uiaction.StateAwaitingApproval, true),
		uiaction.DisplayCopy(uiaction.StateAwaitingApproval, false),
		"the design spec makes 'you must approve' and 'someone else must' different messages")
}

// TestDisplayCopyForNoRecordIsSilent pins the arm every OTHER assertion in
// this file deliberately excludes. The zero State means "no record exists
// yet" — nobody has clicked this control, or the newest record names another
// action or another viewer — and it is what an `action:` data binding
// resolves to on a Tier-0 page's very first paint, before any interaction.
// It must render as NOTHING.
//
// It used to render as the literal string "Unknown state." in front of a
// viewer, and the resolver's own test could not tell: it asserted only
// NotEmpty, which that string satisfies.
func TestDisplayCopyForNoRecordIsSilent(t *testing.T) {
	for _, addressed := range []bool{true, false} {
		assert.Equal(t, "", uiaction.DisplayCopy("", addressed),
			"no record yet must paint nothing, not a sentence about a state nothing reached")
	}
	assert.Equal(t, "", uiaction.DisplayCopy(uiaction.State("something_a_newer_build_writes"), false),
		"an unrecognized state is a claim this build cannot make; a diagnostic must never reach a viewer")
}
