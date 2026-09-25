package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestIdentityChoiceDecisionHandler_ActionOutcomes is a table-driven unit
// test of the handler in isolation (no pipe, no registry) — the pure
// action→Outcome mapping table_driven per AGENTS.md's "3+ cases share shape"
// rule.
func TestIdentityChoiceDecisionHandler_ActionOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		actionID   string
		wantResult string
		wantErr    bool
	}{
		{name: "agent: Approved, OutcomeText=agent", actionID: "agent", wantResult: channelevents.OutcomeApproved},
		{name: "userPassthrough: Approved, OutcomeText=userPassthrough", actionID: "userPassthrough", wantResult: channelevents.OutcomeApproved},
		{name: "cancel: Denied, OutcomeText=cancel", actionID: "cancel", wantResult: channelevents.OutcomeDenied},
		{name: "unknown action: error, zero Outcome", actionID: "sudo", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := IdentityChoiceDecisionHandler(context.Background(), channelinteractions.Decision{
				Payload: channelevents.InteractionDecisionPayload{Category: categories.IdentityChoice, RequestRef: "req-x", ActionID: tc.actionID},
			})
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "unknown action")
				assert.Zero(t, out)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantResult, out.Result)
			assert.Equal(t, tc.actionID, out.OutcomeText, "OutcomeText carries the raw action id")
			require.NoError(t, out.Validate(), "handler must return a valid Outcome")
		})
	}
}

// bindRealIdentityChoiceCategory registers the REAL identity_choice category
// row (mirroring categories.go's shape exactly, not a fixture) and Binds the
// REAL IdentityChoiceDecisionHandler — proving the actual production wiring
// end-to-end through the generic HandleInteractionDecision pipe, not a spy.
//
// The registry/bindings maps are process-global (pkg/channels/channelinteractions),
// so every test in this package that touches them resets on both sides —
// mirroring registerInteractionCategory/resetInteractions in
// interaction_decision_test.go. A bare channelinteractions.Reset() wipes the
// categories package's own init()-time registration too, so this helper
// re-registers the real row rather than assuming it survives the test binary's
// other Reset() calls.
func bindRealIdentityChoiceCategory(t *testing.T) {
	t.Helper()
	resetInteractions(t)
	channelinteractions.Register(channelinteractions.Category{
		Name:      categories.IdentityChoice,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideRequester,
		Resurface: channelinteractions.ResurfaceCached,
	})
	channelinteractions.Bind(categories.IdentityChoice, IdentityChoiceDecisionHandler)
}

// requesterScopedIdentityChoiceRequest returns a minimal AudienceRequester-scope
// InteractionRequestPayload naming requester as the addressee — the shape the
// DecideRequester standing check reads from the parked-prompt record.
func requesterScopedIdentityChoiceRequest(requester channelevents.ExternalIdentity) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceRequester,
			Requester: &requester,
		},
	}
}

// Case: the requester picks "userPassthrough" — the bound handler runs,
// Applied is published on BOTH .in (runner resume) and .out (surface ack)
// with Outcome=Approved and OutcomeText="userPassthrough" (the encoding the
// runner's subscribeInteractionApplied reads the 3-way action back out of).
func TestIdentityChoiceDecisionHandler_RequesterUserPassthrough_AppliedApprovedWithActionInOutcomeText(t *testing.T) {
	bindRealIdentityChoiceCategory(t)

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec) // DecideRequester never touches Engine
	mem := newTestMemory(t)
	p.Mem = mem
	requester := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, categories.IdentityChoice, "req-1", requesterScopedIdentityChoiceRequest(requester))

	env := mustBuildInteractionDecision(t, sessKey, categories.IdentityChoice, "req-1", "userPassthrough", requester)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")

	inPL := findInteractionAppliedPayload(t, natsRec, ".in")
	assert.Equal(t, categories.IdentityChoice, inPL.Category, "in Applied.Category")
	assert.Equal(t, channelevents.OutcomeApproved, inPL.Outcome, "in Applied.Outcome")
	assert.Equal(t, "userPassthrough", inPL.OutcomeText, "in Applied.OutcomeText carries the 3-way action (no Action field on Applied)")
	assert.Equal(t, "req-1", inPL.RequestRef)

	outPL := findInteractionAppliedPayload(t, natsRec, ".out")
	assert.Equal(t, channelevents.OutcomeApproved, outPL.Outcome, "out Applied.Outcome")
	assert.Equal(t, "userPassthrough", outPL.OutcomeText, "out Applied.OutcomeText carries the 3-way action")

	assert.Empty(t, outstandingPrompts(t, p, sessKey), "pending prompt cleared once resolved")
}

// Case: "cancel" maps to OutcomeDenied (not Approved) — the rendering choice
// documented on IdentityChoiceDecisionHandler — but still carries "cancel"
// verbatim in OutcomeText so the gate's Eval (which switches on the raw
// action, not Outcome) sees the right value.
func TestIdentityChoiceDecisionHandler_Cancel_AppliedDeniedWithActionInOutcomeText(t *testing.T) {
	bindRealIdentityChoiceCategory(t)

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	requester := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, categories.IdentityChoice, "req-2", requesterScopedIdentityChoiceRequest(requester))

	env := mustBuildInteractionDecision(t, sessKey, categories.IdentityChoice, "req-2", "cancel", requester)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env))

	outPL := findInteractionAppliedPayload(t, natsRec, ".out")
	assert.Equal(t, channelevents.OutcomeDenied, outPL.Outcome, "cancel renders as denied")
	assert.Equal(t, "cancel", outPL.OutcomeText, "raw action still recoverable regardless of Outcome")
}

// Case: an out-of-vocabulary action — the handler errors, HandleInteractionDecision
// propagates it, and nothing is published. A malformed/forged decision must
// not resolve the gate.
func TestIdentityChoiceDecisionHandler_UnknownAction_ErrorsAndPublishesNothing(t *testing.T) {
	bindRealIdentityChoiceCategory(t)

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	requester := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, categories.IdentityChoice, "req-3", requesterScopedIdentityChoiceRequest(requester))

	env := mustBuildInteractionDecision(t, sessKey, categories.IdentityChoice, "req-3", "sudo", requester)
	err := p.HandleInteractionDecision(context.Background(), env)
	require.Error(t, err, "unknown action must error")
	assert.Contains(t, err.Error(), "unknown action")
	// A bound-handler error is surfaced to the clicker as a handler_error
	// rejection AND propagated as an error — no Applied, but never silent.
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied published for an unknown action")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "an unknown action publishes a handler_error rejection")
	assert.Equal(t, "handler_error", findInteractionDecisionRejected(t, natsRec).Class, "handler-error rejection class")
}

// Case: identity_choice-specific fail-closed coverage — a decider who is NOT
// the prompt's addressee must be rejected before the bound handler ever runs.
// Proven through the REAL category + REAL handler, not the generic fixture
// category interaction_decision_test.go covers this behavior with.
func TestIdentityChoiceDecisionHandler_NonRequesterDecider_FailsClosed(t *testing.T) {
	bindRealIdentityChoiceCategory(t)

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	requester := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, categories.IdentityChoice, "req-4", requesterScopedIdentityChoiceRequest(requester))

	notRequester := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MALLORY", Email: "mallory@example.com"}
	env := mustBuildInteractionDecision(t, sessKey, categories.IdentityChoice, "req-4", "agent", notRequester)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"a requester mismatch is a fail-closed reject, not a plumbing error")

	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied published for a non-requester decider")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a non-requester decider gets a not_authorized rejection")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
		"pending prompt survives so the real requester can still resolve it")
}
