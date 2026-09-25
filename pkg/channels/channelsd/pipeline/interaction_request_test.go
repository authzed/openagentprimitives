package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	citest "github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/testsupport"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// registerContentInspectionCategory installs a snapshot guard and asserts the
// production content_inspection category (Park=AwaitingDecision, DecideApprovers,
// ResurfaceCached — the shape these park tests exercise) is registered.
// content_inspection is a real production row (categories.init()), so this must
// NOT register its own copy: that would panic on a duplicate. The snapshot is
// still needed — tests that Bind content_inspection rely on its cleanup (Reset
// + ResetBindings + re-register saved) to avoid a cross-test double-bind. Never
// a bare Reset.
func registerContentInspectionCategory(t *testing.T) {
	t.Helper()
	citest.WithRegistrySnapshot(t)
	c, ok := channelinteractions.Get("content_inspection")
	require.True(t, ok, "content_inspection must be a registered production category (blank-import categories)")
	require.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision, c.Park, "content_inspection parks at AwaitingDecision")
	require.Equal(t, channelinteractions.DecideApprovers, c.Deciders, "content_inspection decides via approvers")
	require.Equal(t, channelinteractions.ResurfaceCached, c.Resurface, "content_inspection resurfaces cached")
}

// ciInteractionRequest builds a minimal, valid content_inspection
// interaction_request payload keyed on reqID (approvers audience).
func ciInteractionRequest(reqID string) channelevents.InteractionRequestPayload {
	return channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "c1-abc"},
		Category:        "content_inspection",
		RequestRef:      reqID,
		Lead:            "Possible prompt injection — web_fetch output",
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "slack", Subject: "user:owner@example.com"}},
		},
	}
}

// seedPendingInteraction installs a PendingInteraction on the fixture
// AgentSession through the same WriteOwned path production uses.
func seedPendingInteraction(t *testing.T, cli client.Client, key client.ObjectKey, pi spiceboxv1alpha1.PendingInteraction) {
	t.Helper()
	var sess spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), key, &sess), "Get before seed")
	cp := sess.DeepCopy()
	cp.Status.PendingInteractions = append(cp.Status.PendingInteractions, pi)
	seedApprovalStatus(t, cli, cp)
}

// TestHandleInteractionRequest_WritesEntryAndReEmits: a first
// interaction_request parks a self-contained PendingInteraction entry, derives
// the approver subject from the category's DeciderPolicy, copies the Lead into
// Summary, re-emits the request on OUT for the generic renderer — and writes NO
// phase-deriving status condition (addendum C-1).
func TestHandleInteractionRequest_WritesEntryAndReEmits(t *testing.T) {
	registerContentInspectionCategory(t)
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{}, natsRec)

	env, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionRequest, ciInteractionRequest("req-1"))
	require.NoError(t, err, "build envelope")

	require.NoError(t, p.HandleInteractionRequest(context.Background(), env),
		"HandleInteractionRequest")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKey, &got), "Get session")
	require.Len(t, got.Status.PendingInteractions, 1,
		"one PendingInteraction parked: %+v", got.Status.PendingInteractions)
	pi := got.Status.PendingInteractions[0]
	assert.Equal(t, "req-1", pi.RequestID, "RequestID")
	assert.Equal(t, "req-1", pi.RequestRef, "RequestRef")
	assert.Equal(t, "content_inspection", pi.Category, "Category")
	assert.Equal(t, "agentsession:default/c1-abc#approve", pi.ApproverSubject, "ApproverSubject")
	assert.Equal(t, "Possible prompt injection — web_fetch output", pi.Summary, "Summary = Lead")

	// C-1 invariant: the park handler writes no PHASE-deriving condition —
	// phase=AwaitingDecision still comes from the runner's signed lifecycle
	// projection, independent of this handler. It DOES restore the parallel
	// OBSERVABLE condition its category declares (Category.PendingCondition), the
	// surface the deleted typed handlers always wrote. content_inspection declares
	// ContentInspectionApprovalPending, so the parked session carries it True.
	ciCond := conditions.Find(got.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionContentInspectionApprovalPending)
	require.NotNil(t, ciCond,
		"content_inspection park must set its observable ContentInspectionApprovalPending condition")
	assert.Equal(t, metav1.ConditionTrue, ciCond.Status,
		"ContentInspectionApprovalPending=True while the interaction is parked")

	// Re-emitted on OUT for the generic renderer.
	var sawOut bool
	for _, s := range natsRec.subjects {
		if strings.HasSuffix(s, ".out.interaction_request") {
			sawOut = true
		}
	}
	assert.True(t, sawOut, "must re-emit interaction_request on OUT; subjects=%v", natsRec.subjects)
}

// TestHandleInteractionRequest_DedupsOnRequestID: the runner re-publishes an
// outstanding request on restart; the same RequestID must not add a second
// PendingInteractions entry.
func TestHandleInteractionRequest_DedupsOnRequestID(t *testing.T) {
	registerContentInspectionCategory(t)
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	p := newTestPipeline(t, cli, &fakeAuthz{}, &fakeNATS{})

	env, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionRequest, ciInteractionRequest("req-1"))
	require.NoError(t, err)

	require.NoError(t, p.HandleInteractionRequest(context.Background(), env), "first park")
	require.NoError(t, p.HandleInteractionRequest(context.Background(), env), "runner-restart re-publish")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKey, &got))
	assert.Len(t, got.Status.PendingInteractions, 1, "dedup on RequestID: no duplicate entry")
}

// TestHandleInteractionApplied_ClearsOnTimeout: the runner's gate-side timeout
// watcher publishes interaction_applied(expired) on IN; this clears the durable
// park record so it doesn't leak.
func TestHandleInteractionApplied_ClearsOnTimeout(t *testing.T) {
	registerContentInspectionCategory(t)
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	p := newTestPipeline(t, cli, &fakeAuthz{}, &fakeNATS{})

	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID: "req-1", RequestRef: "req-1", Category: "content_inspection",
		RequestedAt: metav1.Now(),
	})

	applied := channelevents.InteractionAppliedPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessKey.Namespace, Name: sessKey.Name},
		Category:        "content_inspection",
		RequestRef:      "req-1",
		Outcome:         channelevents.OutcomeExpired,
	}
	env, err := channelevents.BuildEnvelope(sessKey.Namespace, sessKey.Name,
		channelevents.KindInteractionApplied, applied)
	require.NoError(t, err)

	require.NoError(t, p.HandleInteractionApplied(context.Background(), env))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKey, &got))
	assert.Empty(t, got.Status.PendingInteractions, "timeout applied clears the entry")
}

// TestHandleInteractionDecision_ClearsDurablePendingInteraction: a resolved
// decision on a parked content_inspection request must clear the durable
// Status.PendingInteractions entry HandleInteractionRequest wrote, not only
// tombstone the parked_prompt record. Without this every parking approval
// category leaks a stranded PendingInteractions entry per successful decision.
func TestHandleInteractionDecision_ClearsDurablePendingInteraction(t *testing.T) {
	registerContentInspectionCategory(t)
	channelinteractions.Bind("content_inspection", ApprovalDecisionHandler)

	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	sessKey := sessKeyForFixture()
	authz := &fakeAuthz{checkApproveResult: true}
	natsRec := &fakeNATS{}
	p := newTestPipeline(t, cli, authz, natsRec)
	mem := newTestMemory(t)
	p.Mem = mem

	seedPendingInteraction(t, cli, sessKey, spiceboxv1alpha1.PendingInteraction{
		RequestID: "req-1", RequestRef: "req-1", Category: "content_inspection",
		RequestedAt: metav1.Now(),
	})
	approver := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, sessKey, "content_inspection", "req-1", approversFixtureRequest(approver))

	env := mustBuildInteractionDecision(t, sessKey, "content_inspection", "req-1", "approve", approver)
	require.NoError(t, p.HandleInteractionDecision(context.Background(), env), "HandleInteractionDecision")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKey, &got))
	assert.Empty(t, got.Status.PendingInteractions, "decision must clear the durable PendingInteractions entry")
}
