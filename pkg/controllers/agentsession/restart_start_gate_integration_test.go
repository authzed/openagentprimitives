//go:build integration

package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestReconcileRestart_TakeoverStartGate is the security regression this round
// closes: takeover mode deliberately SKIPS the SessionFork gate (Step 1.5,
// below) on the premise that channelsd's app-mention is "the same basis on
// which the requester could start their own session" — but a class's
// allowedStarters is exactly what invalidates that premise for a taker who
// isn't listed. Without the class start gate checked first, a non-listed
// takeover would land started_by (⇒ interact ⇒ read_transcript) on a child
// seeded with a copy of the parent's transcript, with no pod ever running and
// nothing else in the path to catch it. Runs against real SpiceDB so the
// "no started_by" assertion proves the actual permission semantics, not a
// fake's approximation.
func TestReconcileRestart_TakeoverStartGate(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	spdb, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spdb.Close() })
	ctx, cancel := context.WithTimeout(memory.WithSystemApproval(context.Background(), "test"), 30*time.Second)
	defer cancel()

	const ns = "integration-test"
	owner := uniqLeak(t, "owner-")
	listed := uniqLeak(t, "listed-")
	stranger := uniqLeak(t, "stranger-")

	// fixture builds one gated class (allowedStarters = [listed]) and one
	// terminal parent session with a signed takeover marker naming `who` as
	// the requester, ready for ReconcileRestart.
	fixture := func(t *testing.T, who string) (cb *fake.ClientBuilder, parent *spiceboxv1alpha1.AgentSession, childName string) {
		t.Helper()
		class := uniqLeak(t, "gated-")
		childName = uniqLeak(t, "child-")
		require.NoError(t, spdb.EnsureAgentClassPlatform(ctx, ns, class))
		require.NoError(t, spdb.EnsureAgentClassStarters(ctx, ns, class, []string{"user:" + listed}))
		ac := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: class, Namespace: ns},
			Spec: spiceboxv1alpha1.AgentClassSpec{Authz: &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
				AllowedStarters: []string{"user:" + listed},
			}}},
			// Attests to the EnsureAgentClassStarters above: the restart gate,
			// like EnforceStartGate, requeues rather than checks while the
			// class's starter set is unconfirmed.
			Status: spiceboxv1alpha1.AgentClassStatus{Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentClassConditionStartersLinked,
				Status:             metav1.ConditionTrue,
				Reason:             spiceboxv1alpha1.ReasonStartersLinked,
				LastTransitionTime: metav1.Now(),
			}}},
		}
		parentName := uniqLeak(t, "parent-")
		parent = &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: parentName, Namespace: ns,
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + owner},
			},
			Spec: spiceboxv1alpha1.AgentSessionSpec{Class: class},
			Status: spiceboxv1alpha1.AgentSessionStatus{
				Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
				PendingRestart: &spiceboxv1alpha1.PendingRestart{
					Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
					InheritHistory:    false, // policy/security-halt carve-out shape: cut=-1, no parent turns needed
					NewUserText:       "taking this over",
					TriggeredBy:       identity.Subject("user:" + who),
					TargetSessionName: childName,
				},
			},
		}
		armSignedRestart(t, parent)
		return fake.NewClientBuilder().WithScheme(restartScheme(t)).WithObjects(parent, ac).
			WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}), parent, childName
	}

	t.Run("takeover by a non-listed stranger is denied before any materialization", func(t *testing.T) {
		cb, parent, childName := fixture(t, stranger)
		c := cb.Build()
		mem := memory.NewLocal(inmem.NewBackend())
		r := &agentsession.Reconciler{
			Client:            c,
			RestartMemory:     mem,
			AuthzGranter:      spdb,
			DeniedLister:      spdb,
			ForkChecker:       spdb,
			StartChecker:      spdb,
			Snapshotter:       &recordingSnap{},
			ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
			PublisherKeys:     testMarkerKeys,
		}

		proceed, _, err := r.ReconcileRestart(ctx, parent)
		require.NoError(t, err, "a definitive refusal is not an error — it's a recorded denial")
		assert.False(t, proceed)

		// No child CR.
		childGetErr := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: childName}, &spiceboxv1alpha1.AgentSession{})
		assert.True(t, apierrors.IsNotFound(childGetErr), "a denied takeover must create no child AgentSession")

		// No memory copy: the child scope carries no turns.
		childScope := memory.Scope{Kind: "session", ID: ns + "/" + childName}
		childTurns, terr := turn.ReadAll(ctx, mem, childScope)
		require.NoError(t, terr)
		assert.Empty(t, childTurns, "a denied takeover must copy no transcript into the child scope")

		// No SpiceDB tuple: the stranger has no interact (started_by would
		// otherwise grant it) on the never-materialized child object.
		assert.False(t, checkPerm(ctx, t, spdb, "agentsession", ns+"/"+childName, "interact", stranger),
			"a denied takeover must write no started_by (or any) tuple naming the child")

		// The parent carries the refusal, worded for the person, not SpiceDB.
		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: parent.Name}, &got))
		assert.Nil(t, got.Status.PendingRestart, "PendingRestart cleared on deny (no retry loop)")
		cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRestartDenied)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionNotAnAllowedStarter, cond.Reason)
		assert.Equal(t, "This agent only runs for the people on its list, and you're not on it yet. Ask an administrator of this agent to add you.", cond.Message)
	})

	t.Run("takeover by a listed starter proceeds and materializes the child", func(t *testing.T) {
		cb, parent, childName := fixture(t, listed)
		c := cb.Build()
		mem := memory.NewLocal(inmem.NewBackend())
		r := &agentsession.Reconciler{
			Client:            c,
			RestartMemory:     mem,
			AuthzGranter:      spdb,
			DeniedLister:      spdb,
			ForkChecker:       spdb,
			StartChecker:      spdb,
			Snapshotter:       &recordingSnap{},
			ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
			PublisherKeys:     testMarkerKeys,
		}

		// proceed is false on this call regardless of outcome — ReconcileRestart
		// always short-circuits the rest of Reconcile once PendingRestart is set
		// (see its call site in controller.go); a completed fork is told apart
		// from a denial by status, not by proceed. err == nil is what says this
		// call was not a hard failure.
		_, _, err := r.ReconcileRestart(ctx, parent)
		require.NoError(t, err)

		var child spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: childName}, &child),
			"a listed starter's takeover must materialize the child")

		assert.True(t, checkPerm(ctx, t, spdb, "agentsession", ns+"/"+childName, "interact", listed),
			"the listed starter must hold interact (started_by) on the child it now owns")

		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: parent.Name}, &got))
		assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionRestartDenied),
			"a listed starter's takeover must not be recorded as a restart denial")
		assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSupersededByRestart),
			"the fork must have run to completion (parent superseded), not merely avoided a denial")
	})
}
