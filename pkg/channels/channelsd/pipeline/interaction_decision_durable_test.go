package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// Cross-restart idempotency. The in-process resolvedCache is the warm
// already-resolved gate, and NewPipeline builds it empty; every other input a
// re-decision needs survives durably. So without a durable gate, a second
// approver's click after a restart re-runs the bound handler in full — for
// tool_approval a fresh SpiceDB grant tuple (TTL=0 = a session-wide backstop
// that silently auto-approves the next identical call) plus a second, possibly
// contradicting, interaction_applied.
//
// Second clicks are ordinary: sendRequest fans one prompt out per approver and
// only the clicker's copy is edited, so approvers 2..n keep armed buttons — and
// after a restart the delivery store is empty, so not even the recorded copies
// can be edited.
//
// The durable witness is the parked-prompt tombstone clearPendingPrompt writes
// on the first decision. This test models the restart the only way that matters:
// a SECOND Pipeline over the SAME durable stores.
func TestHandleInteractionDecision_AfterRestart_DurablyResolvedPrompt_IsSpectator(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	mem := newTestMemory(t)

	// --- channelsd process #1: approver A decides. ---
	firstNATS := &fakeNATS{}
	p1 := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, firstNATS)
	p1.Mem = mem
	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(alice))
	require.NoError(t, p1.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"first decision (approver A)")
	require.Equal(t, 1, calls, "the first decision runs the bound handler")
	require.Empty(t, outstandingPrompts(t, p1, sessKey), "the first decision resolves the parked prompt")

	// --- channelsd restarts: a brand-new Pipeline, empty resolvedCache, same
	// durable memory and same API objects. Approver B's card is still armed. ---
	secondNATS := &fakeNATS{}
	p2 := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, secondNATS)
	p2.Mem = mem
	bob := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_BOB", Email: "bob@example.com"}
	require.NoError(t, p2.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", bob)),
		"post-restart decision (approver B)")

	assert.Equal(t, 1, calls,
		"the bound handler must NOT re-run after a restart: a second grant write is a session-wide backstop nothing revokes")
	assert.False(t, sawPublishedKind(secondNATS, channelevents.KindInteractionApplied),
		"no second interaction_applied — the runner already resumed on the first one")
	rej := findInteractionDecisionRejected(t, secondNATS)
	assert.Equal(t, "already_resolved", rej.Class, "the post-restart click is a spectator")
	assert.Equal(t, "req-1", rej.RequestRef, "rejection names the request that was clicked")
	assert.Equal(t, bob.ExternalID, rej.Clicker.ExternalID, "rejection is addressed to the late clicker")
}

// Same hole, no restart needed: the gate-side timeout path clears the pending
// entry and the parked prompt but never touches resolvedCache, so without the
// durable gate a click landing after the timeout runs the handler in full,
// writing a grant for a request the runner already denied.
func TestHandleInteractionDecision_AfterTimeoutApplied_IsSpectator(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, natsRec)
	p.Now = func() time.Time { return time.Now() }
	mem := newTestMemory(t)
	p.Mem = mem

	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(alice))
	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID:   "req-1",
		Category:    fixtureInteractionCategory,
		RequestRef:  "req-1",
		RequestedAt: metav1.NewTime(time.Now()),
	})

	// The runner's gate-side timeout watcher: interaction_applied(expired) on IN.
	expired, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Category:        fixtureInteractionCategory,
			RequestRef:      "req-1",
			Outcome:         channelevents.OutcomeExpired,
		})
	require.NoError(t, err, "build timeout applied envelope")
	require.NoError(t, p.HandleInteractionApplied(context.Background(), expired), "timeout applied")
	require.Empty(t, outstandingPrompts(t, p, sessKey), "the timeout path resolves the parked prompt")

	// The approver clicks Approve on their still-armed card, after the timeout.
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"post-timeout click")

	assert.Equal(t, 0, calls,
		"the bound handler must NOT run after the gate timed out — the grant would outlive a request the runner already denied")
	for _, published := range natsRec.payloads {
		var envelope channelevents.Envelope
		require.NoError(t, json.Unmarshal(published, &envelope))
		if envelope.Kind == channelevents.KindInteractionApplied {
			var applied channelevents.InteractionAppliedPayload
			require.NoError(t, json.Unmarshal(envelope.Payload, &applied))
			assert.Equal(t, channelevents.OutcomeExpired, applied.Outcome)
		}
	}
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "expired", rej.Class,
		"the post-timeout click is told the request expired, not that someone else decided it")
}

// A category whose prompt was never durably noted (ResurfaceRegenerate, or a
// note that failed) must be unaffected: absence of a record is NOT a verdict.
// This is the fail-closed regression the audit's own fix sketch would have
// caused — it proposed keying on the absence of a status.PendingInteractions
// entry, which conflates "resolved" with "never parked".
func TestHandleInteractionDecision_NoDurablePromptRecord_StillDecides(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, natsRec)
	p.Mem = newTestMemory(t) // empty: no parked-prompt record at all

	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"decision with no durable prompt record")

	assert.Equal(t, 1, calls, "an un-noted prompt must still decide — absence of a record is not a resolution")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "Applied still publishes")
}

// The timeout can reach channelsd BEFORE the prompt does. The runner reports
// the expiry on IN, which channelsd handles promptly, while the prompt itself
// rides the OUT relay — observed on a live cluster running six minutes behind.
// HandleInteractionApplied then finds no parked record to tombstone, the relay
// notes the prompt afterward as live, and the card the approver finally sees
// stays armed: a Slack approver card is an ephemeral, which chat.update cannot
// edit to "expired". A click on it ran the handler in full — "approved", a grant
// written — for a request the runner had abandoned minutes earlier, and nothing
// ever acted on it.
//
// The request's own ExpiresAt is the witness that does not depend on that
// ordering, so a click past it is refused and the prompt tombstoned.
func TestHandleInteractionDecision_PastExpiresAt_NotedAfterTimeout_IsRejectedExpired(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, natsRec)
	now := time.Now()
	p.Now = func() time.Time { return now }
	mem := newTestMemory(t)
	p.Mem = mem

	// 1. The runner's deadline passes; its expiry lands with nothing noted yet.
	expired, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Category:        fixtureInteractionCategory,
			RequestRef:      "req-1",
			Outcome:         channelevents.OutcomeExpired,
		})
	require.NoError(t, err, "build timeout applied envelope")
	require.NoError(t, p.HandleInteractionApplied(context.Background(), expired), "timeout applied")

	// 2. The lagging relay notes the prompt afterward, as live.
	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	req := approversFixtureRequest(alice)
	deadline := now.Add(-8 * time.Minute)
	req.ExpiresAt = &deadline
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", req)
	natsRec.subjects, natsRec.payloads = nil, nil

	// 3. The approver clicks the still-armed card.
	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"click past the request's deadline")

	assert.Equal(t, 0, calls, "the bound handler must NOT run for a request whose deadline has passed")
	assert.Equal(t, []string{channelevents.OutcomeExpired}, publishedAppliedOutcomes(t, natsRec, ".out."),
		"the only interaction_applied retires the card as expired: nothing was decided")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "expired", rej.Class, "the clicker is told the request expired")
	assert.Equal(t, alice.ExternalID, rej.Clicker.ExternalID, "rejection is addressed to the clicker")
	assert.Empty(t, outstandingPrompts(t, p, sessKey), "the expired prompt is tombstoned so it is never re-surfaced")
}

// The deadline check must not refuse a click that lands in time.
func TestHandleInteractionDecision_BeforeExpiresAt_StillDecides(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, natsRec)
	now := time.Now()
	p.Now = func() time.Time { return now }
	mem := newTestMemory(t)
	p.Mem = mem

	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	req := approversFixtureRequest(alice)
	deadline := now.Add(time.Minute)
	req.ExpiresAt = &deadline
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", req)

	require.NoError(t, p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", alice)),
		"click before the deadline")

	assert.Equal(t, 1, calls, "a click inside the window decides")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "Applied publishes")
}

// publishedAppliedOutcomes returns the Outcome of every interaction_applied
// natsRec recorded on subjects containing dir (".in." or ".out."), in order.
func publishedAppliedOutcomes(t *testing.T, natsRec *fakeNATS, dir string) []string {
	t.Helper()
	var outcomes []string
	for i, s := range natsRec.subjects {
		if !strings.Contains(s, dir) || !strings.HasSuffix(s, "."+string(channelevents.KindInteractionApplied)) {
			continue
		}
		var envelope channelevents.Envelope
		require.NoError(t, json.Unmarshal(natsRec.payloads[i], &envelope), "unmarshal applied envelope")
		var applied channelevents.InteractionAppliedPayload
		require.NoError(t, json.Unmarshal(envelope.Payload, &applied), "unmarshal applied payload")
		outcomes = append(outcomes, applied.Outcome)
	}
	return outcomes
}

// expiryFixture is a pipeline over one parked approvers-policy prompt req-1
// whose deadline is expiresIn from the pinned clock (nil: no deadline), plus a
// matching PendingInteractions entry on the session.
type expiryFixture struct {
	p       *Pipeline
	cli     client.Client
	natsRec *fakeNATS
	sessKey client.ObjectKey
	now     time.Time
	calls   *int
}

func newExpiryFixture(t *testing.T, authorized bool, expiresIn *time.Duration) expiryFixture {
	t.Helper()
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := new(int)
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(calls, channelinteractions.Outcome{
		Result: channelevents.OutcomeApproved,
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: authorized}, natsRec)
	now := time.Now()
	p.Now = func() time.Time { return now }
	mem := newTestMemory(t)
	p.Mem = mem

	req := approversFixtureRequest(expiryAlice)
	if expiresIn != nil {
		deadline := now.Add(*expiresIn)
		req.ExpiresAt = &deadline
	}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", req)
	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID:   "req-1",
		Category:    fixtureInteractionCategory,
		RequestRef:  "req-1",
		RequestedAt: metav1.NewTime(now),
	})
	return expiryFixture{p: p, cli: cli, natsRec: natsRec, sessKey: sessKey, now: now, calls: calls}
}

var expiryAlice = channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}

func (f expiryFixture) click(t *testing.T, actionID string) channelevents.InteractionDecisionRejectedPayload {
	t.Helper()
	f.natsRec.subjects, f.natsRec.payloads = nil, nil
	require.NoError(t, f.p.HandleInteractionDecision(context.Background(),
		mustBuildInteractionDecision(t, f.sessKey, fixtureInteractionCategory, "req-1", actionID, expiryAlice)),
		"click %q", actionID)
	if !sawPublishedKind(f.natsRec, channelevents.KindInteractionDecisionRejected) {
		return channelevents.InteractionDecisionRejectedPayload{}
	}
	return findInteractionDecisionRejected(t, f.natsRec)
}

func (f expiryFixture) pendingInteractions(t *testing.T) []spiceboxv1alpha1.PendingInteraction {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, f.cli.Get(context.Background(), f.sessKey, &sess), "Get session")
	return sess.Status.PendingInteractions
}

// The deadline gate's boundaries and its place after the standing checks.
func TestHandleInteractionDecision_DeadlineGate(t *testing.T) {
	ago, atDeadline := -time.Minute, time.Duration(0)
	cases := []struct {
		name       string
		authorized bool
		expiresIn  *time.Duration
		wantClass  string // "" = decided, no rejection
		wantCalls  int
		wantParked bool
	}{
		{name: "click exactly at ExpiresAt: rejected expired, prompt retired", authorized: true, expiresIn: &atDeadline, wantClass: "expired"},
		{name: "no ExpiresAt: decides", authorized: true, wantCalls: 1},
		{name: "past ExpiresAt, no standing: not_authorized, prompt left parked", authorized: false, expiresIn: &ago, wantClass: "not_authorized", wantParked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExpiryFixture(t, tc.authorized, tc.expiresIn)
			rej := f.click(t, "approve")
			assert.Equal(t, tc.wantClass, rej.Class)
			assert.Equal(t, tc.wantCalls, *f.calls)
			assert.Equal(t, tc.wantParked, len(outstandingPrompts(t, f.p, f.sessKey)) > 0, "prompt still parked")
			assert.Equal(t, tc.wantParked, len(f.pendingInteractions(t)) > 0, "PendingInteractions entry still present")
		})
	}
}

// A click past the deadline resolves the request the way the runner's timeout
// would: the expiry is published, both ways, and the PendingInteractions entry
// is dropped. When the runner's own timeout report lands afterward it must not
// be swallowed by a bare tombstone, and must not publish a second verdict.
func TestHandleInteractionDecision_PastExpiresAt_ThenRunnerTimeout_StillPublishesExpiredOut(t *testing.T) {
	ago := -time.Minute
	f := newExpiryFixture(t, true, &ago)

	rej := f.click(t, "approve")
	require.Equal(t, "expired", rej.Class, "the late click is refused as expired")
	assert.Equal(t, []string{channelevents.OutcomeExpired}, publishedAppliedOutcomes(t, f.natsRec, ".out."),
		"other viewers' cards are retired as expired")
	assert.Equal(t, []string{channelevents.OutcomeExpired}, publishedAppliedOutcomes(t, f.natsRec, ".in."),
		"the runner is told the request expired")
	assert.Empty(t, f.pendingInteractions(t), "the PendingInteractions entry is dropped")

	f.natsRec.subjects, f.natsRec.payloads = nil, nil
	timeout, err := channelevents.BuildEnvelope(f.sessKey.Namespace, f.sessKey.Name,
		channelevents.KindInteractionApplied,
		channelevents.InteractionAppliedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: f.sessKey.Namespace, Name: f.sessKey.Name},
			Category:        fixtureInteractionCategory,
			RequestRef:      "req-1",
			Outcome:         channelevents.OutcomeExpired,
			Reason:          "timeout",
		})
	require.NoError(t, err, "build timeout applied envelope")
	require.NoError(t, f.p.HandleInteractionApplied(context.Background(), timeout), "runner timeout after the click")
	assert.Empty(t, publishedAppliedOutcomes(t, f.natsRec, ""), "the expiry already went out; no second verdict")
}

// Every click after the first expired one, Approve or Deny, is told the
// request expired — not that someone else resolved it — and decides nothing.
func TestHandleInteractionDecision_PastExpiresAt_LaterClicksStayExpired(t *testing.T) {
	ago := -time.Minute
	f := newExpiryFixture(t, true, &ago)

	for _, action := range []string{"approve", "approve", "deny"} {
		rej := f.click(t, action)
		assert.Equal(t, "expired", rej.Class, "click %q", action)
		assert.Equal(t, expiredDecisionReason, rej.Reason, "click %q", action)
	}
	assert.Equal(t, 0, *f.calls, "the bound handler never runs")
}
