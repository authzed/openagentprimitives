// pkg/channels/channelsd/pipeline/interaction_loop_test.go
//
// End-to-end loop proof for the generic Interaction model: a request envelope
// reaches the fake kind's "interaction" sub-channel sender (the same dispatch
// the outbound relay performs — see pkg/channels/channelsd/outbound/relay.go's
// KindInteractionRequest / KindInteractionApplied case), a decision drives
// HandleInteractionDecision, the resulting Applied lands on both the
// runner-facing (.in) and surface-facing (.out) subjects, the OUT Applied
// reaches the same fake sub-channel sender, and the parked prompt is cleared.
//
// It does not spin up the real outbound.Relay/NATS subscription; it proves the
// two halves (relay-style routing into the fake recorder; decision pipe →
// Applied recorded on out) against ONE shared memory facade, which is the
// production wiring: the relay writes the durable parked_prompt record and the
// decision pipe reads it and lays down its RESOLVED tombstone.
package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

// loopCatName is the registry key the loop-proof test registers under —
// distinct from fixtureInteractionCategory (interaction_decision_test.go) so
// the two files' package-global registry state never collides even though
// they reset it independently.
const loopCatName = "loop_cat"

// TestInteractionLoop_RequestThroughFakeRecorder_DecisionThroughPipe_AppliedThroughFakeRecorder
// pins the whole loop for a ResurfaceCached / DecideApprovers category:
// Lead/Actions/RequestRef round-trip through the "interaction" sub-channel
// sender, the durable parked_prompt record holds the request until a decision
// arrives, the decision runs the bound handler exactly once and publishes
// Applied on both .in and .out, and the record stops being outstanding once
// Applied is published.
func TestInteractionLoop_RequestThroughFakeRecorder_DecisionThroughPipe_AppliedThroughFakeRecorder(t *testing.T) {
	fakekind.ResetAllDrivers()
	t.Cleanup(fakekind.ResetAllDrivers)

	// --- Step 1: register + bind the loop category. ---
	resetInteractions(t)
	channelinteractions.Register(channelinteractions.Category{
		Name:      loopCatName,
		Park:      spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideApprovers,
		Resurface: channelinteractions.ResurfaceCached,
	})
	handlerCalls := 0
	channelinteractions.Bind(loopCatName, trackingHandler(&handlerCalls, channelinteractions.Outcome{
		Result:      channelevents.OutcomeApproved,
		OutcomeText: "Approved by the loop-proof approver",
		Reason:      "looks fine",
	}, nil))

	// --- Step 2: fake-kind Channel + AgentSession fixture, shared memory. ---
	fakeCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "loop-ch"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t) // the relay writes and the pipeline reads the SAME durable record
	p.Mem = mem

	approver := channelevents.ExternalIdentity{Kind: "fake", ExternalID: "U_ALICE", Email: "alice@example.com"}
	requestRef := "loop-req-1"
	reqPayload := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
		Category:        loopCatName,
		RequestRef:      requestRef,
		Lead:            "Approve the loop-proof action?",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStylePrimary},
			{ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision, Style: channelevents.ActionStyleDanger},
		},
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{approver},
		},
	}
	reqEnv, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name, channelevents.KindInteractionRequest, reqPayload)
	require.NoError(t, err, "build interaction request envelope")

	// --- Step 3: relay-style dispatch — the same sub-channel name + sender the
	// outbound relay resolves via SubChannelSenderFor(ctx, sess, "interaction"). ---
	sender := fakekind.Kind{}.SubChannelSender("interaction", channelkinds.Deps{Channel: fakeCh})
	require.NotNil(t, sender, `fake kind must implement the "interaction" sub-channel`)
	sessInfo := channelkinds.SessionInfo{Namespace: sessKey.Namespace, Name: sessKey.Name}
	_, err = sender.Send(context.Background(), sessInfo, reqEnv)
	require.NoError(t, err, "fake interaction sender: send request")

	// Relay-side durable record: mirrors notePendingPrompt's
	// KindInteractionRequest branch (pkg/channels/channelsd/outbound/relay.go) — a
	// ResurfaceCached category's request is recorded so a re-interaction, or a
	// surface attaching after a restart, can re-surface it. The writer is the
	// relay, not the pipeline, so the test must write it here.
	notePendingInteractionRequest(t, mem, sessKey, loopCatName, requestRef, reqPayload)

	prompts := fakekind.DriverFor(fakeCh.Namespace, fakeCh.Name).InteractionPrompts()
	require.Len(t, prompts, 1, "the interaction request must be recorded by the fake sub-channel sender")
	assert.Equal(t, reqPayload.Lead, prompts[0].Payload.Lead, "Lead round-trips")
	assert.Equal(t, reqPayload.Actions, prompts[0].Payload.Actions, "Actions round-trip")
	assert.Equal(t, requestRef, prompts[0].Payload.RequestRef, "RequestRef round-trips")
	assert.Equal(t, sessInfo, prompts[0].SessionRef, "the SessionInfo the sender was called with is recorded")

	require.Len(t, outstandingPrompts(t, p, sessKey), 1, "the durable record holds the request before a decision arrives")

	// --- Step 4: decision → pipe → Applied. ---
	decisionEnv, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionDecision,
		channelevents.InteractionDecisionPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
			Category:        loopCatName,
			RequestRef:      requestRef,
			ActionID:        "approve",
			Decider:         approver,
			ResponseRef:     "https://hooks.fake.example/" + requestRef,
		})
	require.NoError(t, err, "build interaction decision envelope")

	require.NoError(t, p.HandleInteractionDecision(context.Background(), decisionEnv), "HandleInteractionDecision")
	assert.Equal(t, 1, handlerCalls, "bound handler must run exactly once")

	inEnv := findEnvelopeBySubjectSuffix(t, natsRec, ".in."+string(channelevents.KindInteractionApplied))
	var inPL channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(inEnv.Payload, &inPL))
	assert.Equal(t, channelevents.OutcomeApproved, inPL.Outcome, "in Applied.Outcome")
	assert.Equal(t, requestRef, inPL.RequestRef, "in Applied.RequestRef")

	outEnv := findEnvelopeBySubjectSuffix(t, natsRec, ".out."+string(channelevents.KindInteractionApplied))
	var outPL channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(outEnv.Payload, &outPL))
	assert.Equal(t, channelevents.OutcomeApproved, outPL.Outcome, "out Applied.Outcome")
	assert.Equal(t, "Approved by the loop-proof approver", outPL.OutcomeText, "out Applied.OutcomeText from the bound handler's Outcome")

	// Relay-delivering the OUT Applied: SubChannelSenderFor resolves the same
	// "interaction" sub-channel for KindInteractionApplied, so the fake sender
	// records it into the same Driver the request landed in.
	_, err = sender.Send(context.Background(), sessInfo, outEnv)
	require.NoError(t, err, "fake interaction sender: send applied")

	applieds := fakekind.DriverFor(fakeCh.Namespace, fakeCh.Name).InteractionApplieds()
	require.Len(t, applieds, 1, "the OUT Applied must be recorded by the fake sub-channel sender")
	assert.Equal(t, channelevents.OutcomeApproved, applieds[0].Payload.Outcome)
	assert.Equal(t, requestRef, applieds[0].Payload.RequestRef)

	// HandleInteractionDecision tombstones the same durable parked_prompt
	// record the relay wrote, so the request must stop being outstanding —
	// otherwise every surface that later attaches re-posts a decided prompt.
	assert.Empty(t, outstandingPrompts(t, p, sessKey),
		"pending prompt must be cleared once Applied is published")
	assert.Empty(t, outstandingPrompts(t, p, sessKey),
		"the same registry instance reflects the clear regardless of which field name reads it")
}

// findEnvelopeBySubjectSuffix returns the decoded Envelope for the first
// natsRec publish whose subject ends with suffix, or fails the test.
// Mirrors findInteractionAppliedPayload (interaction_decision_test.go) but
// returns the whole envelope so the caller can re-dispatch it through a
// Sender, as the loop proof does for the OUT leg.
func findEnvelopeBySubjectSuffix(t *testing.T, natsRec *fakeNATS, suffix string) channelevents.Envelope {
	t.Helper()
	for i, s := range natsRec.subjects {
		if !strings.HasSuffix(s, suffix) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(natsRec.payloads[i], &env), "unmarshal envelope")
		return env
	}
	t.Fatalf("no envelope with subject suffix %q found; subjects=%v", suffix, natsRec.subjects)
	return channelevents.Envelope{}
}
