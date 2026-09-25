package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fixtureInteractionCategory is the registry key every test in this file
// registers under. Tests never run in parallel within this package, so the
// package-global channelinteractions registry/bindings maps are safe to
// reset per-test (see resetInteractions).
const fixtureInteractionCategory = "fixture_interaction"

// resetInteractions clears the process-global channelinteractions registry
// and bindings before and after the test — mirrors
// channelinteractions/decision_test.go's bindableCategory helper. Every case
// that touches the registry must reset on both sides since it is
// package-level state shared across the whole test binary. The category
// registry (unlike bindings, which the binary Binds at startup rather than
// registering via init()) holds init()-registered production categories, so
// a bare Reset() would clobber them for the rest of the test binary — snapshot
// before clearing and restore in Cleanup.
func resetInteractions(t *testing.T) {
	t.Helper()
	saved := channelinteractions.All()
	channelinteractions.Reset()
	channelinteractions.ResetBindings()
	t.Cleanup(func() {
		channelinteractions.Reset()
		for _, c := range saved {
			channelinteractions.Register(c)
		}
		channelinteractions.ResetBindings()
	})
}

// registerInteractionCategory resets the registry then registers
// fixtureInteractionCategory, parked at AwaitingDecision, with the given
// decider policy. Does NOT bind a handler — callers needing a bound handler
// call channelinteractions.Bind themselves (so the "unbound category" case
// can register without binding).
func registerInteractionCategory(t *testing.T, deciders channelinteractions.DeciderPolicy) channelinteractions.Category {
	t.Helper()
	resetInteractions(t)
	c := channelinteractions.Category{
		Name:      fixtureInteractionCategory,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  deciders,
		Resurface: channelinteractions.ResurfaceCached,
	}
	channelinteractions.Register(c)
	return c
}

// trackingHandler returns a channelinteractions.DecisionHandler that
// increments *calls on every invocation and returns (out, err) verbatim.
func trackingHandler(calls *int, out channelinteractions.Outcome, err error) channelinteractions.DecisionHandler {
	return func(_ context.Context, _ channelinteractions.Decision) (channelinteractions.Outcome, error) {
		*calls++
		return out, err
	}
}

// mustBuildInteractionDecision builds a valid KindInteractionDecision envelope.
func mustBuildInteractionDecision(t *testing.T, sessKey client.ObjectKey, category, requestRef, actionID string, decider channelevents.ExternalIdentity) channelevents.Envelope {
	t.Helper()
	env, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionDecision,
		channelevents.InteractionDecisionPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Category:        category,
			RequestRef:      requestRef,
			ActionID:        actionID,
			Decider:         decider,
			ResponseRef:     "https://hooks.slack.example/" + requestRef,
		})
	require.NoError(t, err, "build interaction decision envelope")
	return env
}

// notePendingInteractionRequest seeds mem with the durable parked_prompt record
// for (sessKey, requestRef) — the record HandleInteractionDecision reads both
// for its Decision.Request field and for the DecideRequester standing check,
// and the one resurfacePending's cached leg republishes.
// category/requestRef/AgentSessionRef are stamped onto req from the other args;
// Lead defaults to a fixture value when the caller left it blank
// (InteractionRequestPayload.Validate isn't exercised by BuildEnvelope, but a
// non-empty Lead keeps the fixture realistic).
func notePendingInteractionRequest(t *testing.T, mem memory.Memory, sessKey client.ObjectKey, category, requestRef string, req channelevents.InteractionRequestPayload) {
	t.Helper()
	req.AgentSessionRef = channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name}
	req.Category = category
	req.RequestRef = requestRef
	if req.Lead == "" {
		req.Lead = "fixture prompt"
	}
	env, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name, channelevents.KindInteractionRequest, req)
	require.NoError(t, err, "build interaction request envelope")
	raw, err := json.Marshal(env)
	require.NoError(t, err, "marshal interaction request envelope")
	// Interruptible is propagated from the payload, mirroring the outbound
	// relay's notePendingPrompt (pkg/channels/channelsd/outbound/relay.go) — the stored
	// Content.Interruptible is what republishPrompt reads to decide whether to
	// stamp a fresh interrupt id, not the payload field itself.
	require.NoError(t, parkedprompt.Note(context.Background(), mem,
		promptScope(sessKey.Namespace, sessKey.Name), parkedprompt.Content{
			RequestRef:    requestRef,
			Category:      category,
			Interruptible: req.Interruptible,
			Envelope:      raw,
		}), "note parked prompt")
}

// findInteractionAppliedPayload returns the first KindInteractionApplied
// envelope recorded by natsRec on the given subject suffix (.in or .out) —
// or fails the test if none is present. Mirrors findAppliedPayload in
// tool_approval_test.go.
func findInteractionAppliedPayload(t *testing.T, natsRec *fakeNATS, subjectSuffix string) channelevents.InteractionAppliedPayload {
	t.Helper()
	for i, s := range natsRec.subjects {
		if !strings.HasSuffix(s, subjectSuffix+"."+string(channelevents.KindInteractionApplied)) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(natsRec.payloads[i], &env), "unmarshal envelope")
		var pl channelevents.InteractionAppliedPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal applied payload")
		return pl
	}
	t.Fatalf("no %s.interaction_applied envelope found; subjects=%v", subjectSuffix, natsRec.subjects)
	return channelevents.InteractionAppliedPayload{}
}

// approversFixtureRequest returns a minimal AudienceApprovers-scope
// InteractionRequestPayload naming approver as the sole listed approver
// (delivery-routing only — CheckApproverAuthorized re-validates server-side).
func approversFixtureRequest(approver channelevents.ExternalIdentity) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{approver},
		},
	}
}

// Case 1: happy path — bound handler called; Applied published on BOTH in+out
// subjects with Outcome/DecidedBy/ResponseRef; pending prompt cleared.
func TestHandleInteractionDecision_HappyPath_AppliedBothSubjectsAndClearsPending(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{
		Result:      channelevents.OutcomeApproved,
		OutcomeText: "Approved by alice",
		Reason:      "looks good",
	}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(approver))

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", approver)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")

	assert.Equal(t, 1, calls, "bound handler must be called exactly once")

	inPL := findInteractionAppliedPayload(t, natsRec, ".in")
	assert.Equal(t, channelevents.OutcomeApproved, inPL.Outcome, "in Applied.Outcome")
	assert.Equal(t, "req-1", inPL.RequestRef, "in Applied.RequestRef")
	require.NotNil(t, inPL.DecidedBy, "in Applied.DecidedBy must be set")
	assert.Equal(t, "U_ALICE", inPL.DecidedBy.ExternalID.String(), "in Applied.DecidedBy.ExternalID")
	assert.Equal(t, "https://hooks.slack.example/req-1", inPL.ResponseRef, "in Applied.ResponseRef round-trips the decision's ResponseRef")

	outPL := findInteractionAppliedPayload(t, natsRec, ".out")
	assert.Equal(t, channelevents.OutcomeApproved, outPL.Outcome, "out Applied.Outcome")
	assert.Equal(t, "Approved by alice", outPL.OutcomeText, "out Applied.OutcomeText from handler Outcome")

	assert.Empty(t, outstandingPrompts(t, p, sessKey),
		"pending prompt must be cleared once Applied is published")
}

// Case 2: second decision for the same requestRef — handler NOT called
// again; no second Applied; spectator (idempotent).
func TestHandleInteractionDecision_SecondDecision_IsSpectator(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	// resolvedCache's TTL check always compares against the real wall clock
	// (time.Now(), not p.Now()); the fixture's stubbed Now (2026-04-28, from
	// newTestPipeline) is far enough in the past that an entry stamped with it
	// would read back as already-expired on the very next Get. Use the real
	// clock here so the idempotency check under test isn't defeated by the
	// unrelated fixed-time stub — mirrors the same real-time seeding rationale
	// in TestHandleToolApprovalDecision_AlreadyResolved_PublishesSpectator.
	p.Now = func() time.Time { return time.Now() }
	mem := newTestMemory(t)
	p.Mem = mem
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(approver))

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", approver)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "first HandleInteractionDecision")
	require.Equal(t, 1, calls, "handler called on the first decision")
	require.Equal(t, 2, len(natsRec.subjects), "first decision publishes Applied on both in+out")
	firstPublishCount := len(natsRec.subjects)

	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "second HandleInteractionDecision (late/duplicate click)")
	assert.Equal(t, 1, calls, "handler must not be invoked a second time for an already-resolved requestRef")
	// The spectator path is never silent: exactly one per-clicker
	// already_resolved rejection (OUT only), never a second Applied.
	assert.Equal(t, firstPublishCount+1, len(natsRec.subjects), "spectator publishes one rejection, no second Applied")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "already_resolved", rej.Class, "spectator rejection class")
	require.NotNil(t, rej.OriginalDecider, "already_resolved names the original decider")
	assert.Equal(t, approver.ExternalID, rej.OriginalDecider.ExternalID, "original decider carried on the rejection")
	assert.Equal(t, string(channelevents.OutcomeApproved), rej.OriginalOutcome, "original outcome carried on the rejection")
}

// Case 1b (async suppress): a handler returning Outcome{Suppressed:true}
// resolves out-of-band (queued_messages fires a KindInterruptRequest and the
// bridge publishes the applied later). The pipe MUST NOT publish the
// synchronous interaction_applied — neither on IN nor on OUT — but MUST still
// dedupe the decision (a second click is a spectator that does not re-fire the
// handler) and clear the pending prompt (it has been acted on).
func TestHandleInteractionDecision_SuppressedOutcome_NoAppliedPublishedButDedupedAndCleared(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideParticipant)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Suppressed: true}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkResult: true} // DecideParticipant → interact standing granted
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	// resolvedCache's TTL check compares against the real wall clock, not p.Now
	// (whose fixed 2026-04-28 stub would read back as already-expired) — seed the
	// real clock so the dedup assertion below isn't defeated by the time stub.
	p.Now = func() time.Time { return time.Now() }
	mem := newTestMemory(t)
	p.Mem = mem
	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{})

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "interrupt", decider)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")

	assert.Equal(t, 1, calls, "the suppress handler must be invoked exactly once")
	assert.Empty(t, natsRec.subjects,
		"a suppressed outcome must publish ZERO interaction_applied (neither IN nor OUT) — the bridge resolves it later")
	assert.Empty(t, outstandingPrompts(t, p, sessKey),
		"the pending prompt must be cleared even for a suppressed outcome (it has been acted on)")

	// Second click for the same requestRef: deduped via resolvedCache — the
	// handler must NOT fire a second interrupt.
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "second (duplicate) decision")
	assert.Equal(t, 1, calls, "a suppressed decision must be deduped: a second click does not re-fire the handler")
	// The duplicate click is a spectator on the suppressed decision's
	// resolvedCache entry: it gets a per-clicker already_resolved rejection,
	// but still no second Applied and no second handler run.
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "still no interaction_applied after the duplicate click")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "the duplicate click publishes an already_resolved rejection")
}

// Case 3: unknown category — error returned, no publish.
func TestHandleInteractionDecision_UnknownCategory_Errors(t *testing.T) {
	resetInteractions(t) // nothing registered

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)

	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	env := mustBuildInteractionDecision(t, sessKey, "no_such_category", "req-1", "approve", decider)
	err := p.HandleInteractionDecision(context.Background(), env)
	require.Error(t, err, "expected error for an unregistered category")
	assert.Contains(t, err.Error(), "unknown category")
	assert.Empty(t, natsRec.subjects, "no publish for an unknown category")
}

// Case 4: unbound category (registered, no Bind) — error "no bound decision
// handler", no publish.
func TestHandleInteractionDecision_UnboundCategory_Errors(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers) // registered, deliberately NOT bound

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true} // standing check must pass so the pipe reaches the Bind lookup
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)

	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", decider)
	err := p.HandleInteractionDecision(context.Background(), env)
	require.Error(t, err, "expected error for a registered-but-unbound category")
	assert.Contains(t, err.Error(), "no bound decision handler")
	assert.Empty(t, natsRec.subjects, "no publish for an unbound category")
}

// Case 5: DecideApprovers + engine says not authorized — handler NOT called,
// no Applied, pending prompt NOT cleared.
func TestHandleInteractionDecision_ApproversNotAuthorized_Rejects(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: false} // clicker lacks agentsession#approve
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	unauthorized := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_RANDOM", Email: "random@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(unauthorized))

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", unauthorized)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"an unauthorized decider is a fail-closed reject, not a handler/plumbing error")

	assert.Equal(t, 0, calls, "handler must not be called for an unauthorized decider")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish for an unauthorized decider")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "an unauthorized decider gets a not_authorized rejection")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
		"pending prompt must survive an unauthorized decision so the real approver can still resolve it")
}

// Case 6: DecideRequester + parked request whose Audience.Requester
// canonical matches the decider — handler called.
func TestHandleInteractionDecision_RequesterMatches_HandlerCalled(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideRequester)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec) // DecideRequester never touches Engine
	mem := newTestMemory(t)
	p.Mem = mem
	requesterID := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &requesterID},
	})

	// Same canonical identity as the cached requester — Canonical() keys on
	// email when present, so kind/teamScope/externalID needn't also match.
	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "acknowledge", decider)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")
	assert.Equal(t, 1, calls, "handler must be called when the decider matches the cached requester")
}

// Case 7: DecideRequester + NO parked request to read the addressee from — a
// forged or stale requestRef, or a category that parks nothing. There is
// nothing to check the decider against, so the only safe answer is a
// fail-closed reject: handler NOT called.
func TestHandleInteractionDecision_RequesterNoCachedRequest_FailsClosed(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideRequester)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)
	p.Mem = newTestMemory(t) // empty: no parked request for req-1

	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "acknowledge", decider)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"a fail-closed reject on a cache-miss must not itself surface as an error")
	assert.Equal(t, 0, calls, "handler must not be called with no cached request to verify the requester against")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a cache-miss requester check publishes a not_authorized rejection")
}

// Case 8: handler returns error — no Applied published, pending prompt NOT
// cleared, error propagated.
func TestHandleInteractionDecision_HandlerErrors_NoApplied(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	handlerErr := errors.New("downstream write failed")
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{}, handlerErr))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(approver))

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", approver)
	err := p.HandleInteractionDecision(context.Background(), env)
	require.Error(t, err, "handler error must propagate")
	assert.Contains(t, err.Error(), "downstream write failed")
	assert.Equal(t, 1, calls, "handler was invoked")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish on handler error")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a handler error is surfaced to the clicker as a handler_error rejection AND propagated")
	rej := findInteractionDecisionRejected(t, natsRec)
	assert.Equal(t, "handler_error", rej.Class, "handler-error rejection class")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1, "pending prompt survives a handler error")
}

// Case 9: handler returns an Outcome failing Validate() — treated as a
// handler error (same shape as case 8).
func TestHandleInteractionDecision_HandlerReturnsInvalidOutcome_TreatedAsError(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: "not_a_real_outcome"}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(approver))

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "approve", approver)
	err := p.HandleInteractionDecision(context.Background(), env)
	require.Error(t, err, "an invalid Outcome must be treated as a handler error")
	assert.Equal(t, 1, calls, "handler was invoked")
	assert.Empty(t, natsRec.subjects, "no Applied publish for an invalid outcome")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1, "pending prompt survives an invalid-outcome handler result")
}

// Case 10: the decision claims a DIFFERENT registered category than the
// parked request it targets by requestRef — e.g. a stale/forged click
// resolving one prompt using another category's decider policy. Category match
// must be enforced EXPLICITLY, not left to Audience-shape coincidence. Handler
// NOT called, no Applied, parked prompt survives.
func TestHandleInteractionDecision_CategoryMismatchesCachedRequest_FailsClosed(t *testing.T) {
	resetInteractions(t)
	const otherCategory = "fixture_interaction_other"
	channelinteractions.Register(channelinteractions.Category{
		Name:      fixtureInteractionCategory,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideApprovers,
		Resurface: channelinteractions.ResurfaceCached,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      otherCategory,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideApprovers,
		Resurface: channelinteractions.ResurfaceCached,
	})
	calls := 0
	// Bind only otherCategory's handler — the decision below claims
	// otherCategory, so if the mismatch guard were missing this would be the
	// handler invoked.
	channelinteractions.Bind(otherCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true} // would authorize if the mismatch guard didn't reject first
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	// Cached request is for fixtureInteractionCategory ...
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", approversFixtureRequest(approver))

	// ... but the decision claims otherCategory, for the SAME requestRef.
	env := mustBuildInteractionDecision(t, sessKey, otherCategory, "req-1", "approve", approver)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"a category mismatch is a fail-closed reject, not a plumbing error")

	assert.Equal(t, 0, calls, "handler must not be called when the decision's category mismatches the cached request's category")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish for a category mismatch")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a category mismatch publishes a category_mismatch rejection")
	assert.Equal(t, "category_mismatch", findInteractionDecisionRejected(t, natsRec).Class, "category-mismatch rejection class")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
		"pending prompt must survive a category-mismatch rejection")
}

// Case 11: DecideRequester + parked request whose Audience.Requester
// canonical does NOT match the decider — a different, otherwise-valid
// identity clicking someone else's requester-scoped prompt. Distinct from
// Case 7 (no parked request at all): here the record IS present and the
// decider DOES canonicalize to a real identity, just not the one the prompt
// was addressed to, so this exercises interaction_decision.go's
// `reqCanon != deciderCanon` branch specifically, not the `req == nil` /
// `Requester == nil` fail-closed branch above it. Without that comparison
// any authenticatable user could resolve any other user's requester-scoped
// prompt. Handler NOT called, no Applied, pending prompt survives.
func TestHandleInteractionDecision_RequesterMismatch_FailsClosed(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideRequester)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec) // DecideRequester never touches Engine
	mem := newTestMemory(t)
	p.Mem = mem
	requesterID := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &requesterID},
	})

	// A DIFFERENT, otherwise-perfectly-valid canonicalizable identity — not
	// the requester the prompt was addressed to.
	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_MALLORY", Email: "mallory@example.com"}
	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "acknowledge", decider)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"a requester mismatch is a fail-closed reject, not a plumbing error")

	assert.Equal(t, 0, calls, "handler must not be called when the decider is not the cached request's addressee")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish for a requester mismatch")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a requester mismatch publishes a not_authorized rejection")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
		"pending prompt must survive so the real addressee can still resolve it")
}

// Case 14: DecideParticipant + engine says the decider has interact
// standing on the session — handler called, Applied published. Wires
// through p.Engine.CheckSessionInteract, which the fakeAuthz fixture
// backs already (engine.Deps.SessionInteractChecker == az in
// newTestPipeline; CheckSessionInteract delegates to az.CheckInteract,
// driven by fakeAuthz.checkResult/checkErr) — no fake changes needed.
func TestHandleInteractionDecision_ParticipantHasStanding_HandlerCalled(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideParticipant)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkResult: true} // CheckSessionInteract -> allowed
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	// DecideParticipant deliberately never reads the parked request (it
	// checks session interact standing on the decider only) — an empty
	// payload proves the switch case doesn't dereference req/req.Audience.
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{})

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "retry", decider)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")

	assert.Equal(t, 1, calls, "handler must be called when the decider has session interact standing")
	assert.Equal(t, 1, authz.checkCalls, "CheckSessionInteract must be invoked exactly once")
	outPL := findInteractionAppliedPayload(t, natsRec, ".out")
	assert.Equal(t, channelevents.OutcomeResolved, outPL.Outcome, "Applied outcome published")
}

// Case 15: DecideParticipant + engine says the decider LACKS interact
// standing — fail-closed reject: handler not called, no Applied, pending
// prompt survives.
func TestHandleInteractionDecision_ParticipantLacksStanding_Rejects(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideParticipant)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkResult: false} // CheckSessionInteract -> not allowed
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_RANDOM", Email: "random@example.com"}
	// DecideParticipant deliberately never reads the parked request (it
	// checks session interact standing on the decider only) — an empty
	// payload proves the switch case doesn't dereference req/req.Audience.
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{})

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "retry", decider)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"a decider lacking interact standing is a fail-closed reject, not a plumbing error")

	assert.Equal(t, 0, calls, "handler must not be called when the decider lacks session interact standing")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish for a decider lacking standing")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a decider lacking interact standing gets a not_authorized rejection")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
		"pending prompt must survive so a standing participant can still resolve it")
}

// Case 16: DecideParticipant + the engine errors on the standing check —
// the error propagates (unlike the false/no-standing case, which is a
// silent fail-closed reject); no Applied published.
func TestHandleInteractionDecision_ParticipantStandingCheckErrors_Errors(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideParticipant)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	checkErr := errors.New("spicedb: deadline exceeded")
	authz := &fakeAuthz{checkErr: checkErr}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem
	decider := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_SAM", Email: "sam@example.com"}
	// DecideParticipant deliberately never reads the parked request (it
	// checks session interact standing on the decider only) — an empty
	// payload proves the switch case doesn't dereference req/req.Audience.
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{})

	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "retry", decider)
	err := p.HandleInteractionDecision(context.Background(), env)
	require.Error(t, err, "a standing-check error must propagate, not fail-closed-reject silently")
	assert.Contains(t, err.Error(), "spicedb: deadline exceeded")
	assert.Equal(t, 0, calls, "handler must not be called when the standing check errors")
	assert.Empty(t, natsRec.subjects, "no Applied publish when the standing check errors")
}

// Case 12/13: identity_choice's real end-to-end shape. Every DecideRequester
// fixture above (Cases 6, 7, 11) hand-injects an Email onto the parked
// Requester, which proves nothing about whether the real gate can produce a
// requester that canonicalizes. These two cases instead build the Requester
// from the SAME two sources IdentityChoiceGate.requester()
// (pkg/agent/runner/identitygate.go) reads — AnnotationStartedByExternalID and
// AnnotationStartedByEmail — for a webchat session, against a decider shaped
// exactly like a real webchat click (pkg/web/webui/chat/identity.go's
// deriveIdentity feeding pkg/channels/channelkinds/browser/listener.go).
//
// LOAD-BEARING: Kind intentionally differs between the two sides — "browser"
// (webui/chat/session.go stamps InputChannel.Kind = browser.KindName) for the
// requester vs "idp" (deriveIdentity's hardcoded Kind) for the decider.
// identity.Principal.Canonical keys purely on Email once it is set (Kind /
// TeamScope / ExternalID are consulted only for the synthetic,
// AllowSynthetic-gated encoding — see pkg/platform/identity/principal.go), so
// the Kind mismatch is harmless as long as Email matches. That is why carrying
// Email on Audience.Requester is sufficient, without normalizing Kind across
// surfaces.

// Case 12: the legitimate requester's own decision is accepted.
func TestHandleInteractionDecision_IdentityChoiceGateShapedRequester_Accepted(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideRequester)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec) // DecideRequester never touches Engine
	mem := newTestMemory(t)
	p.Mem = mem

	// Exactly what requester() emits post-fix for a webchat session: Kind =
	// l.ChannelKind = browser.KindName; ExternalID + Email both sourced from
	// the AgentSession's AnnotationStartedByExternalID / AnnotationStartedByEmail
	// (both hold the initiator's email for webchat).
	requesterID := channelevents.ExternalIdentity{Kind: identity.Kind(browser.KindName), ExternalID: "person@example.org", Email: "person@example.org"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &requesterID},
	})

	// The decider: a real webchat click.
	decider := channelevents.ExternalIdentity{Kind: "idp", ExternalID: "person@example.org", Email: "person@example.org"}
	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "agent", decider)

	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")

	// This is exactly what the pre-fix requester() broke: with Email=="" on
	// requesterID (the old shape), identity.FromExternal("browser", "",
	// "person@example.org", "").Canonical() returns ErrSyntheticSubject (no
	// AllowSynthetic opt-in on the DecideRequester path), interaction_decision.go's
	// `err != nil` branch fires, and the decision is rejected (calls stays 0)
	// no matter who the decider is — every identity_choice decision, from any
	// surface, was unresolvable.
	assert.Equal(t, 1, calls, "the legitimate requester's own decision must be accepted once requester() carries a verified email")
	outPL := findInteractionAppliedPayload(t, natsRec, ".out")
	assert.Equal(t, channelevents.OutcomeResolved, outPL.Outcome, "Applied outcome published")
}

// Case 13: a different, otherwise-legitimate webchat user clicking someone
// else's identity_choice prompt must still be rejected — the fix attaches a
// real email, it does not weaken the requester-equality check itself.
func TestHandleInteractionDecision_IdentityChoiceGateShapedRequester_DifferentUserRejected(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideRequester)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeResolved}, nil))

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem

	requesterID := channelevents.ExternalIdentity{Kind: identity.Kind(browser.KindName), ExternalID: "person@example.org", Email: "person@example.org"}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "req-1", channelevents.InteractionRequestPayload{
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &requesterID},
	})

	decider := channelevents.ExternalIdentity{Kind: "idp", ExternalID: "someone-else@example.org", Email: "someone-else@example.org"}
	env := mustBuildInteractionDecision(t, sessKey, fixtureInteractionCategory, "req-1", "agent", decider)

	require.NoError(t, p.HandleInteractionDecision(context.Background(), env),
		"a requester mismatch is a fail-closed reject, not a plumbing error")
	assert.Equal(t, 0, calls, "a different user's decision on someone else's identity_choice prompt must be rejected")
	assert.False(t, sawPublishedKind(natsRec, channelevents.KindInteractionApplied), "no Applied publish for a mismatched decider")
	assert.True(t, sawPublishedKind(natsRec, channelevents.KindInteractionDecisionRejected), "a mismatched decider gets a not_authorized rejection")
	assert.Len(t, outstandingPrompts(t, p, sessKey), 1,
		"pending prompt must survive so the real addressee can still resolve it")
}

// outstandingPrompts returns the durable parked prompts still awaiting the user
// for sessKey. Assertions on "is the prompt still pending" read the same durable
// record production does, so a test cannot pass against process state that a
// restart would erase.
func outstandingPrompts(t *testing.T, p *Pipeline, sessKey client.ObjectKey) []parkedprompt.Content {
	t.Helper()
	got, err := parkedprompt.Outstanding(context.Background(), p.Mem,
		promptScope(sessKey.Namespace, sessKey.Name))
	require.NoError(t, err, "read outstanding parked prompts")
	return got
}
