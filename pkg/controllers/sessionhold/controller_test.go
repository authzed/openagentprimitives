package sessionhold_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories/sessionrelease"
	"github.com/authzed/openagentprimitives/pkg/controllers/sessionhold"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

const (
	ns          = "demo-ns"
	sessionName = "demo-session"
	holdName    = "demo-hold"
)

func sessionKey() types.NamespacedName { return types.NamespacedName{Namespace: ns, Name: sessionName} }
func holdKey() types.NamespacedName    { return types.NamespacedName{Namespace: ns, Name: holdName} }

// baseSession is a channel-attached AgentSession with a started-by owner —
// what publishCard needs to address the card, and what Decide's
// never-touches-AgentSession-status test needs to prove is left alone.
func baseSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sessionName,
			Namespace: ns,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
				spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "slack", Key: "thread:C1:1"},
		},
	}
}

// activeHold is a SessionHold already stamped Active by the AgentSession
// reconciler's own reconcileHold (pkg/controllers/agentsession/hold.go) —
// this package's Reconciler never stamps Phase itself; it only reacts to an
// already-Active hold.
func activeHold() *spiceboxv1alpha1.SessionHold {
	return &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: holdName, Namespace: ns},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Reason:     "12 consecutive out-of-ceiling calls",
			Source:     "tripper/plangate-denial-streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase: spiceboxv1alpha1.SessionHoldPhaseActive,
		},
	}
}

type recordedEnvelope struct {
	subject string
	data    []byte
}

// failingMemory wraps a real memory.Memory and fails every Put whose Entry
// Kind matches FailKind — used to prove Decide's write-then-flip ordering: if
// the EventApprovalsCleared write fails, the hold must not release.
type failingMemory struct {
	memorypkg.Memory
	failKind string
}

func (f *failingMemory) Put(ctx context.Context, e memorypkg.Entry) (memorypkg.Entry, error) {
	if e.Kind == f.failKind {
		return memorypkg.Entry{}, errors.New("injected memory write failure")
	}
	return f.Memory.Put(ctx, e)
}

// newFixture builds a fake-client + real in-memory Reconciler. Mirrors
// pkg/controllers/credentialupdaterequest's newReconciler shape
// (testfixtures.NewScheme + fake.NewClientBuilder), extended with the memory
// facade and recording NATS publisher this package's Reconciler also needs.
func newFixture(t *testing.T, mem memorypkg.Memory, objs ...client.Object) (client.Client, *sessionhold.Reconciler, *[]recordedEnvelope) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.SessionHold{}).
		Build()
	recs := &[]recordedEnvelope{}
	r := &sessionhold.Reconciler{
		Client: c,
		Memory: mem,
		NATSPublish: func(subject string, data []byte) error {
			*recs = append(*recs, recordedEnvelope{subject, data})
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
	}
	return c, r, recs
}

func getHold(t *testing.T, c client.Client) *spiceboxv1alpha1.SessionHold {
	t.Helper()
	var got spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(context.Background(), holdKey(), &got), "Get SessionHold")
	return &got
}

func reconcileHold(t *testing.T, r *sessionhold.Reconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: holdKey()})
}

// --- Reconcile: publish on first observation ---------------------------

func TestReconcile_activeHoldNoInteractionRef_publishesCardAndRecordsRef(t *testing.T) {
	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, recs := newFixture(t, mem, baseSession(), activeHold())

	_, err := reconcileHold(t, r)
	require.NoError(t, err, "Reconcile")

	require.Len(t, *recs, 1, "exactly one interaction_request published")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal((*recs)[0].data, &env), "unmarshal envelope")
	assert.Equal(t, channelevents.KindInteractionRequest, env.Kind)

	var payload channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &payload), "decode InteractionRequestPayload")
	assert.Equal(t, sessionrelease.CategoryName, payload.Category)
	assert.Equal(t, sessionName, payload.AgentSessionRef.Name)
	assert.NotEmpty(t, payload.RequestRef)

	got := getHold(t, c)
	assert.NotEmpty(t, got.Status.InteractionRef, "InteractionRef recorded")
	assert.Equal(t, payload.RequestRef, got.Status.InteractionRef, "recorded ref matches the published one")
}

func TestReconcile_activeHoldWithInteractionRefAlready_doesNotRepublish(t *testing.T) {
	mem := memorypkg.NewLocal(inmem.NewBackend())
	hold := activeHold()
	hold.Status.InteractionRef = "already-published"
	c, r, recs := newFixture(t, mem, baseSession(), hold)

	_, err := reconcileHold(t, r)
	require.NoError(t, err, "Reconcile")

	assert.Empty(t, *recs, "no second card published")
	got := getHold(t, c)
	assert.Equal(t, "already-published", got.Status.InteractionRef)
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseActive, got.Status.Phase,
		"fails closed: an unanswered card leaves the hold Active, not Released")
}

// --- Decide: approve writes the clear THEN flips Released ---------------

// fixtureHandle is the one permission both the plan-gate fixture below and
// the digest re-derivation agree on, in permsurface's wire spelling (see
// pkg/authz/plangate/drift_test.go's hsIn, which uses the same "perm:"
// prefix).
const fixtureHandle = "perm:read:doc"

func fixturePlan(t *testing.T) plangate.Plan {
	t.Helper()
	h, err := permsurface.ParseHandle(fixtureHandle)
	require.NoError(t, err)
	return plangate.Plan{Phases: []plangate.Phase{
		{Permissions: []permsurface.Handle{h}, Max: plangate.MaxSpec{Count: 1}},
	}}
}

// seedApprovedPlan writes the single plan_gate_audit record a session needs
// for plangate.PlanFromRecords to reconstruct a plan and its digest — the
// same record shape FreezeAndRecordPhases writes in production
// (pkg/agent/runner/pipeline_wiring.go), pared to the one field Decide's
// clear path actually consults.
func seedApprovedPlan(t *testing.T, mem memorypkg.Memory, p plangate.Plan) {
	t.Helper()
	idx := int32(0)
	scope := memorypkg.Scope{Kind: "session", ID: ns + "/" + sessionName}
	require.NoError(t, plangateaudit.Record(context.Background(), mem, scope, plangateaudit.Content{
		Event:      plangateaudit.EventPlanApproved,
		PlanDigest: p.Digest(),
		PhaseIndex: &idx,
		Ceiling:    []string{fixtureHandle},
		Mode:       "enforcing",
	}), "seed EventPlanApproved")
}

// approveDecision's Decider carries an Email, matching the ONE precondition
// production Decide callers always satisfy: the generic decision pipe
// (pkg/channels/channelsd/pipeline/interaction_decision.go) canonicalizes the
// clicker's identity via the same non-synthetic identity.FromExternal(...)
// .Canonical() Decide's own approverSubject uses, and rejects the click
// before ever invoking a bound handler if that fails — an email-less Decider
// can never reach Decide for real. "U-OWNER"/"owner@example.com" mirrors
// baseSession's own started-by annotations, so the approving decider IS this
// session's owner, as sessionrelease.CategoryName's DecideOwner policy
// requires.
func approveDecision(requestRef string) channelinteractions.Decision {
	return channelinteractions.Decision{
		Session: channelevents.SessionRef{Namespace: ns, Name: sessionName},
		Payload: channelevents.InteractionDecisionPayload{
			Category:   sessionrelease.CategoryName,
			RequestRef: requestRef,
			ActionID:   "approve",
			Decider:    channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U-OWNER", Email: "owner@example.com"},
		},
	}
}

func refuseDecision(requestRef string) channelinteractions.Decision {
	d := approveDecision(requestRef)
	d.Payload.ActionID = "refuse"
	return d
}

func TestDecide_approved_writesApprovalsClearedBeforeReleasing(t *testing.T) {
	mem := memorypkg.NewLocal(inmem.NewBackend())
	p := fixturePlan(t)
	seedApprovedPlan(t, mem, p)

	hold := activeHold()
	hold.Status.InteractionRef = "req-1"
	c, r, _ := newFixture(t, mem, baseSession(), hold)

	out, err := r.Decide(context.Background(), approveDecision("req-1"))
	require.NoError(t, err, "Decide")
	assert.Equal(t, channelevents.OutcomeApproved, out.Result)

	// The clear landed, keyed to the session's current plan digest. Reading it
	// back needs the same capability door Decide itself satisfies internally
	// (memory.WithSystemApproval) — this is a plain assertion-side read, not
	// production code, so the test mints its own.
	readCtx := memorypkg.WithSystemApproval(context.Background(), "test")
	scope := memorypkg.Scope{Kind: "session", ID: ns + "/" + sessionName}
	records, err := plangateaudit.List(readCtx, mem, scope)
	require.NoError(t, err)
	var cleared *plangateaudit.Content
	for i := range records {
		if records[i].Event == plangateaudit.EventApprovalsCleared {
			cleared = &records[i]
		}
	}
	require.NotNil(t, cleared, "an EventApprovalsCleared record was written")
	assert.Equal(t, p.Digest(), cleared.PlanDigest, "cleared for the session's actual current plan digest")

	// AND the hold is released, attributed to the approving decider's
	// canonical identity — what the AgentSession reconciler's
	// reconcileHoldRelease reads back to attribute the lifecyclecore.Released
	// event it emits.
	got := getHold(t, c)
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseReleased, got.Status.Phase)
	wantCanon, err := identity.FromExternal(identity.KindSlack, "", identity.RawExternalID("U-OWNER"), identity.Email("owner@example.com")).Canonical()
	require.NoError(t, err)
	assert.Equal(t, wantCanon.Subject(), got.Status.ReleasedBy, "ReleasedBy is the approving decider's canonical subject")
}

// TestDecide_approved_deciderCanonicalizationFails_leavesHoldActive proves
// approverSubject's error path fails closed the same way clearStandingApprovals's
// does: a Decider Decide cannot canonicalize (no email, no synthetic opt-in —
// unreachable from the real decision pipe, which already rejects such a click
// before ever invoking a bound handler, but Decide is unit-tested in
// isolation and must not trust its caller) must error, write nothing, and
// leave the hold Active rather than release with an empty ReleasedBy.
func TestDecide_approved_deciderCanonicalizationFails_leavesHoldActive(t *testing.T) {
	mem := memorypkg.NewLocal(inmem.NewBackend())
	p := fixturePlan(t)
	seedApprovedPlan(t, mem, p)

	hold := activeHold()
	hold.Status.InteractionRef = "req-1"
	c, r, _ := newFixture(t, mem, baseSession(), hold)

	d := approveDecision("req-1")
	d.Payload.Decider = channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U-NOEMAIL"} // no email

	_, err := r.Decide(context.Background(), d)
	require.Error(t, err, "Decide must surface the unresolvable approver identity")

	got := getHold(t, c)
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseActive, got.Status.Phase,
		"an unattributable release must not happen: the hold stays Active")
	assert.Empty(t, got.Status.ReleasedBy, "no ReleasedBy is ever recorded for a failed canonicalization")

	readCtx := memorypkg.WithSystemApproval(context.Background(), "test")
	scope := memorypkg.Scope{Kind: "session", ID: ns + "/" + sessionName}
	records, err := plangateaudit.List(readCtx, mem, scope)
	require.NoError(t, err)
	for _, rec := range records {
		assert.NotEqual(t, plangateaudit.EventApprovalsCleared, rec.Event,
			"standing approvals must not be cleared when the release itself cannot be attributed")
	}
}

// TestDecide_approved_clearFails_leavesHoldActive is the explicit ordering
// proof the brief calls for: if the clear cannot be written, the release
// must not happen either — a release that clears nothing hands the agent
// back the exact ceiling it was frozen with.
func TestDecide_approved_clearFails_leavesHoldActive(t *testing.T) {
	mem := &failingMemory{Memory: memorypkg.NewLocal(inmem.NewBackend()), failKind: plangateaudit.Kind{}.Name()}
	p := fixturePlan(t)
	seedApprovedPlan(t, mem.Memory, p) // seed through the unwrapped facade so the seed itself succeeds

	hold := activeHold()
	hold.Status.InteractionRef = "req-1"
	c, r, _ := newFixture(t, mem, baseSession(), hold)

	_, err := r.Decide(context.Background(), approveDecision("req-1"))
	require.Error(t, err, "Decide must surface the injected write failure")

	got := getHold(t, c)
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseActive, got.Status.Phase,
		"a release that clears nothing must not happen: the hold stays Active")
}

func TestDecide_refused_leavesPhaseActive(t *testing.T) {
	mem := memorypkg.NewLocal(inmem.NewBackend())
	hold := activeHold()
	hold.Status.InteractionRef = "req-1"
	c, r, _ := newFixture(t, mem, baseSession(), hold)

	out, err := r.Decide(context.Background(), refuseDecision("req-1"))
	require.NoError(t, err, "Decide")
	assert.Equal(t, channelevents.OutcomeDenied, out.Result)

	got := getHold(t, c)
	assert.Equal(t, spiceboxv1alpha1.SessionHoldPhaseActive, got.Status.Phase,
		"there is no third state to invent: a refusal simply leaves the hold Active")
}

// TestDecide_neverWritesAgentSessionStatus is the critical invariant: THIS
// controller must never write AgentSession.status — phase is owned solely by
// the AgentSession reconciler, which observes the released hold on its own
// next pass and emits Released itself (see credentialupdate.go's doc on why
// two operator reconcilers must never co-own one CR's phase).
func TestDecide_neverWritesAgentSessionStatus(t *testing.T) {
	mem := memorypkg.NewLocal(inmem.NewBackend())
	p := fixturePlan(t)
	seedApprovedPlan(t, mem, p)

	sess := baseSession()
	hold := activeHold()
	hold.Status.InteractionRef = "req-1"
	c, r, _ := newFixture(t, mem, sess, hold)

	before := sess.DeepCopy()

	_, err := r.Decide(context.Background(), approveDecision("req-1"))
	require.NoError(t, err, "Decide")

	var after spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), sessionKey(), &after), "Get AgentSession")
	assert.Equal(t, before.Status, after.Status, "AgentSession.status is byte-for-byte untouched")
}
