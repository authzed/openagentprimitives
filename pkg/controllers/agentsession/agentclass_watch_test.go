// pkg/controllers/agentsession/agentclass_watch_test.go
//
// Reconcile gates every session on its AgentClass -- ClassResolved=False when
// the class is missing, and again when it is not yet Valid=True -- and returns
// NO RequeueAfter from either branch. Nothing else re-enqueues such a session:
// the AgentClass reconciler writes only its own AgentSessionGrants, the only
// other AgentClass watch belongs to the Channel controller, and the operator
// sets no SyncPeriod (so the fallback is the ~10h default resync).
//
// So the AgentClass watch is not an optimization, it is the only path back for
// a parked session, and its absence is what makes channelsd's user-facing
// promise ("parked sessions resume on their own once the AgentClass is
// Valid=True again", pipeline/agent_unavailable.go) false.
package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// watchScheme returns a scheme carrying the AP API types the watch mapping
// Lists and dispatches on.
func watchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "add spicebox v1alpha1 to scheme")
	return s
}

// classSession builds an AgentSession bound to class, in ns, at phase.
func classSession(t *testing.T, ns, name, class, phase string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

// TestWatchedObjectRequests_AgentClassChange_RequeuesOnlyParkedSessionsOfThatClass
// pins the un-parking contract: an AgentClass write must re-enqueue exactly the
// sessions in its own namespace that name it — terminal ones included — and
// nothing else.
func TestWatchedObjectRequests_AgentClassChange_RequeuesOnlyParkedSessionsOfThatClass(t *testing.T) {
	const (
		ns    = "default"
		class = "demo-class"
	)
	// The session under test: parked because the class was not Valid=True when
	// it last reconciled. Its phase is Pending and its ClassResolved condition
	// records the gate that stopped it.
	parked := classSession(t, ns, "parked-on-class", class, spiceboxv1alpha1.AgentSessionPhasePending)
	parked.Status.Conditions = []metav1.Condition{{
		Type:   spiceboxv1alpha1.AgentSessionConditionClassResolved,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentClassNotValid,
	}}

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: class},
		Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
			Type:   spiceboxv1alpha1.AgentClassConditionValid,
			Status: metav1.ConditionTrue,
			Reason: "AllReferencesResolve",
		}}},
	}

	c := fake.NewClientBuilder().
		WithScheme(watchScheme(t)).
		WithObjects(
			parked,
			// Running session of the same class: also re-enqueued. A class
			// respec (a new tool, a revoked identity) must reach live sessions,
			// not only parked ones.
			classSession(t, ns, "running-same-class", class, spiceboxv1alpha1.AgentSessionPhaseRunning),
			// Terminal sessions of the same class: also re-enqueued. They are
			// past every start/park gate, but not past every class-leveled
			// declaration: spec.authz.session.artifactVisibility re-levels the
			// artifact_org_viewer tuple on completed sessions too — those are
			// exactly the artifacts people share after the fact, and a flip
			// back to "session" must REVOKE on them, not only on live ones.
			classSession(t, ns, "failed-same-class", class, spiceboxv1alpha1.AgentSessionPhaseFailed),
			classSession(t, ns, "succeeded-same-class", class, spiceboxv1alpha1.AgentSessionPhaseSucceeded),
			// A different class in the same namespace: excluded. spec.class is
			// the exact field Reconcile resolves the class by, so unlike the
			// AgentIdentity mapping there is nothing to over-approximate.
			classSession(t, ns, "other-class", "other-class", spiceboxv1alpha1.AgentSessionPhasePending),
			// Same class NAME in another namespace: excluded. AgentClass is
			// namespaced and Reconcile looks it up in the session's own namespace.
			classSession(t, "other-ns", "other-namespace-same-class-name", class, spiceboxv1alpha1.AgentSessionPhasePending),
		).
		Build()

	r := &Reconciler{Client: c}

	got := r.watchedObjectRequests(context.Background(), ac)

	var names []string
	for _, req := range got {
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"parked-on-class", "running-same-class", "failed-same-class", "succeeded-same-class"}, names,
		"an AgentClass write re-enqueues every session in its namespace that names it, terminal ones included (artifactVisibility levels on completed sessions); other-class and other-namespace sessions are excluded")
}

// TestSessionsForAgentClassChange_MatchesNamespaceAndClass exercises the pure
// filter directly, so a regression in the predicate stays distinguishable from
// a regression in the List that feeds it. Phase is deliberately NOT part of
// the predicate: a terminal session still levels artifactVisibility.
func TestSessionsForAgentClassChange_MatchesNamespaceAndClass(t *testing.T) {
	const (
		ns    = "default"
		class = "demo-class"
	)
	items := []spiceboxv1alpha1.AgentSession{
		*classSession(t, ns, "pending", class, spiceboxv1alpha1.AgentSessionPhasePending),
		*classSession(t, ns, "awaiting-credentials", class, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials),
		*classSession(t, ns, "idle", class, spiceboxv1alpha1.AgentSessionPhaseIdle),
		*classSession(t, ns, "failed", class, spiceboxv1alpha1.AgentSessionPhaseFailed),
		*classSession(t, ns, "succeeded", class, spiceboxv1alpha1.AgentSessionPhaseSucceeded),
		*classSession(t, ns, "other-class", "other-class", spiceboxv1alpha1.AgentSessionPhasePending),
		*classSession(t, "other-ns", "other-namespace", class, spiceboxv1alpha1.AgentSessionPhasePending),
	}

	got := sessionsForAgentClassChange(items, ns, class)

	var names []string
	for _, req := range got {
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"pending", "awaiting-credentials", "idle", "failed", "succeeded"}, names,
		"same namespace + same spec.class, every phase")
}
