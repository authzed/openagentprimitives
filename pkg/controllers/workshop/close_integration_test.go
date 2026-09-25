//go:build integration

// The close round trip: one close request driven through the REAL Workshop
// controller against a REAL SpiceDB. The two halves are already covered apart
// — pkg/authz/spicedb's workshop_integration_test.go pins what the tuples mean,
// and close_test.go pins what the pass does with a fake checker — and neither
// can see the join, which is the workshop ID. Provisioning writes the target's
// #starter tuple under the id Reconcile minted (status.namespace), and the
// close pass checks `close` on the id it reads back off the target's status. An
// id that stopped agreeing would leave both suites green and refuse every
// person their own workshop.
//
//	go test -tags=integration -count=1 -run '^TestClose_RealControllerAndSpiceDB$' ./pkg/controllers/workshop/
package workshop_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// gatedTuples wraps the real SpiceDB client and refuses EnsureWorkshopSubjects
// for one workshop id until released. Every other call — including the close
// check this whole test is about — goes straight through to SpiceDB.
//
// Refusing the tuple write is the only way to reach the state a close request
// has to wait on. Reconcile anchors status.namespace at step 3 and writes the
// tuples at step 7, so a workshop whose step 7 fails is ANCHORED with nothing
// standing behind the anchor: failLayer stamps TupleWritten=False, and a check
// against that id would truthfully answer "no" and refuse a person their own
// workshop. Nothing else produces that window on demand.
//
// One goroutine drives every Reconcile here, so blocked needs no lock.
type gatedTuples struct {
	inner   workshop.WorkshopTuples
	blocked string
}

func (g *gatedTuples) EnsureWorkshopSubjects(ctx context.Context, workshopID, sessNS, sessName string, starter identity.CanonicalUserID) error {
	if g.blocked != "" && workshopID == g.blocked {
		return fmt.Errorf("gatedTuples: the test is holding the tuple write for workshop %s", workshopID)
	}
	return g.inner.EnsureWorkshopSubjects(ctx, workshopID, sessNS, sessName, starter)
}

func (g *gatedTuples) CheckWorkshopClose(ctx context.Context, workshopID string, canonical identity.CanonicalUserID) (bool, error) {
	return g.inner.CheckWorkshopClose(ctx, workshopID, canonical)
}

func (g *gatedTuples) DeleteWorkshopRelationships(ctx context.Context, workshopID string) error {
	return g.inner.DeleteWorkshopRelationships(ctx, workshopID)
}

// closeParticipant creates one person's builder AgentSession and its sanctioned
// Workshop, and returns both once the apiserver has assigned the UID every
// derived name (the workshop namespace, and so the workshop id the tuples are
// written under) depends on.
func closeParticipant(t *testing.T, ctx context.Context, c client.Client, name string, starter identity.CanonicalUserID) (*spiceboxv1alpha1.AgentSession, *spiceboxv1alpha1.Workshop) {
	t.Helper()
	sess := builderSession(name, "placeholder-overwritten-by-apiserver")
	sess.Spec = spiceboxv1alpha1.AgentSessionSpec{
		Class:  "workshop-test-class",
		Prompt: spiceboxv1alpha1.PromptSource{Inline: "build something"},
	}
	require.NoError(t, c.Create(ctx, sess), "create AgentSession %s", name)

	ws := sanctionedWorkshop(sess)
	ws.Spec.StarterCanonical = starter.String()
	require.NoError(t, c.Create(ctx, ws), "create Workshop for %s", name)
	return sess, ws
}

// requestClose appends one spec.closeRequests entry and persists it with a
// plain Update on spec — the write the close_others tool makes from inside a
// workshop.
func requestClose(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName, target string) {
	t.Helper()
	var live spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, key, &live), "get requester before writing close request %q", target)
	live.Spec.CloseRequests = append(live.Spec.CloseRequests, spiceboxv1alpha1.WorkshopCloseRequest{
		Target:      target,
		RequestedAt: metav1.NewTime(time.Now()),
	})
	require.NoError(t, c.Update(ctx, &live), "write close request %q", target)
}

// closePass reconciles the requester once and returns the result. Reconcile has
// no already-Ready short circuit, so this re-runs the whole idempotent
// provisioning path and then step 7b, which is the close pass.
func closePass(t *testing.T, ctx context.Context, r *workshop.Reconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "close pass on %s", key)
	return res
}

// closeDecision reads the requester back and returns its decision for target,
// or nil when the pass left the target undecided.
func closeDecision(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName, target string) *spiceboxv1alpha1.WorkshopCloseStatus {
	t.Helper()
	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(ctx, key, &got), "get requester to read its decision for %q", target)
	return closeStatusFor(&got, target)
}

// reconcileUntilTupleWriteHeld drives Reconcile until the held tuple write
// fails the provisioning pass. Bounded rather than a fixed call count for the
// same reason reconcileUntilReady is: the first call only adds the finalizer
// and requeues before any provisioning logic runs.
func reconcileUntilTupleWriteHeld(t *testing.T, ctx context.Context, r *workshop.Reconciler, key types.NamespacedName) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			require.Contains(t, err.Error(), "ensure workshop tuples",
				"the held pass must fail at the TUPLE step, not at an earlier layer: %v", err)
			return
		}
	}
	t.Fatalf("Workshop %s provisioned without the held tuple write ever failing", key)
}

// TestClose_RealControllerAndSpiceDB drives close requests from one workshop
// (A) through the real Workshop controller, with a real *spicedb.Client as its
// WorkshopTuples, against four other workshops: B and C and D belong to the
// same person as A, E to someone else.
//
// The subtests share one envtest cluster and one SpiceDB datastore and run in
// order — each builds on what the previous one decided, which is what makes the
// wildcard's summary a claim about set-once decisions rather than a fresh
// count.
func TestClose_RealControllerAndSpiceDB(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	sc, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err, "dial spicedb")
	t.Cleanup(func() {
		if cerr := sc.Close(); cerr != nil {
			t.Logf("close spicedb client: %v", cerr)
		}
	})

	// Two people. workshop#close is `starter + platform->can_admin` and this
	// datastore has no platform admin, so the starter tuple provisioning wrote
	// is the ONLY thing that can allow a close here.
	builder := identity.CanonicalFromTrusted("builder-user", "test fixture")
	other := identity.CanonicalFromTrusted("builder-x", "test fixture")

	gate := &gatedTuples{inner: sc}
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    gate,
		Tokens:    tokens.NewRegistry(),
	}

	// A is the requester: every close request below is written on A's spec and
	// decided by A's own reconcile, which only reaches the close pass once A is
	// itself fully provisioned.
	_, wsA := closeParticipant(t, ctx, env.Client, "demo-builder-a", builder)
	keyA := client.ObjectKeyFromObject(wsA)
	reconcileUntilReady(t, ctx, r, keyA)

	t.Run("B is provisioned and tupled: the named request is Closed by the real check and B's builder session is deleted", func(t *testing.T) {
		sessB, wsB := closeParticipant(t, ctx, env.Client, "demo-builder-b", builder)
		reconcileUntilReady(t, ctx, r, client.ObjectKeyFromObject(wsB))

		requestClose(t, ctx, env.Client, keyA, wsB.Name)
		res := closePass(t, ctx, r, keyA)

		got := closeDecision(t, ctx, env.Client, keyA, wsB.Name)
		require.NotNil(t, got, "B is decidable, so the pass must have decided it")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, got.Phase)
		assert.Empty(t, got.Message, "an allowed close says nothing; only a refusal carries words")
		assert.NotNil(t, got.DecidedAt)
		assert.False(t, sessionExists(t, env.Client, sessB.Namespace, sessB.Name),
			"closing B deletes B's builder session — that is what ends the workshop")
		assert.Equal(t, time.Hour, res.RequeueAfter,
			"a pass that left nothing undecided must not ask for the short close requeue")
	})

	t.Run("C is anchored but not tupled: the request stays undecided with a short requeue, then is Closed once C's tuples land", func(t *testing.T) {
		sessC, wsC := closeParticipant(t, ctx, env.Client, "demo-builder-c", builder)
		keyC := client.ObjectKeyFromObject(wsC)

		// Hold C's tuple write. The id is the workshop namespace name derived
		// from the session UID the apiserver just assigned — the same value
		// Reconcile computes and writes the tuples under.
		gate.blocked = spiceboxv1alpha1.WorkshopNamespaceName(sessC.UID)
		reconcileUntilTupleWriteHeld(t, ctx, r, keyC)

		var heldC spiceboxv1alpha1.Workshop
		require.NoError(t, env.Client.Get(ctx, keyC, &heldC))
		require.Equal(t, gate.blocked, heldC.Status.Namespace,
			"C must be ANCHORED: the anchor is persisted three steps before the tuples")
		require.False(t, apimeta.IsStatusConditionTrue(heldC.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTupleWritten),
			"C must have no standing tuples, which is the whole point of the held write")

		requestClose(t, ctx, env.Client, keyA, wsC.Name)
		res := closePass(t, ctx, r, keyA)

		assert.Nil(t, closeDecision(t, ctx, env.Client, keyA, wsC.Name),
			"a target whose tuples are not standing must be left undecided, never refused: a set-once refusal here could never be revisited")
		assert.Equal(t, 5*time.Second, res.RequeueAfter,
			"an undecided close request has no trigger of its own, so the pass must ask for its own next look")
		assert.True(t, sessionExists(t, env.Client, sessC.Namespace, sessC.Name),
			"nothing may be ended on an answer nobody could give yet")

		// Let the tuples through. C now looks to the close check exactly as B
		// did, and the SAME undecided request is decided on the next pass.
		gate.blocked = ""
		reconcileUntilReady(t, ctx, r, keyC)
		closePass(t, ctx, r, keyA)

		got := closeDecision(t, ctx, env.Client, keyA, wsC.Name)
		require.NotNil(t, got, "once C's tuples are standing the request must be decided")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, got.Phase)
		assert.False(t, sessionExists(t, env.Client, sessC.Namespace, sessC.Name))
	})

	t.Run("E belongs to another person: the real check refuses it as not theirs to close and E's session survives", func(t *testing.T) {
		sessE, wsE := closeParticipant(t, ctx, env.Client, "demo-builder-e", other)
		reconcileUntilReady(t, ctx, r, client.ObjectKeyFromObject(wsE))

		requestClose(t, ctx, env.Client, keyA, wsE.Name)
		closePass(t, ctx, r, keyA)

		got := closeDecision(t, ctx, env.Client, keyA, wsE.Name)
		require.NotNil(t, got, "a refusal is a decision and must be recorded")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseRefused, got.Phase)
		assert.Equal(t, "not yours to close", got.Message,
			"the words a person reads back through the tool, verbatim")
		assert.True(t, sessionExists(t, env.Client, sessE.Namespace, sessE.Name),
			"a refused close must not have ended anything")
	})

	t.Run("a wildcard expands to the one same-starter workshop nobody had asked about, and its summary counts only what it decided", func(t *testing.T) {
		sessD, wsD := closeParticipant(t, ctx, env.Client, "demo-builder-d", builder)
		reconcileUntilReady(t, ctx, r, client.ObjectKeyFromObject(wsD))

		requestClose(t, ctx, env.Client, keyA, spiceboxv1alpha1.WorkshopCloseTargetAll)
		closePass(t, ctx, r, keyA)

		gotD := closeDecision(t, ctx, env.Client, keyA, wsD.Name)
		require.NotNil(t, gotD, "the wildcard must reach the one target no named request had")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, gotD.Phase)
		assert.False(t, sessionExists(t, env.Client, sessD.Namespace, sessD.Name))

		summary := closeDecision(t, ctx, env.Client, keyA, spiceboxv1alpha1.WorkshopCloseTargetAll)
		require.NotNil(t, summary, "the wildcard answers itself with a summary")
		assert.Equal(t, spiceboxv1alpha1.WorkshopClosePhaseClosed, summary.Phase)
		// One, not three. B and C are the same person's and are still in the
		// expansion — envtest runs no garbage collector, so a closed target's
		// Workshop CR outlives the builder session this pass deleted — but the
		// named requests above already decided them, and a target another
		// request settled is skipped: not re-checked, not re-closed, and not
		// counted into an ask that never decided it. E is not in the expansion
		// at all, being another person's. D is the one workshop this ask
		// decided, so D is the whole of its summary.
		assert.Equal(t, "1 closed, 0 refused", summary.Message)
		assert.Equal(t, spiceboxv1alpha1.WorkshopCloseTargetAll, gotD.Ask,
			"the decision carries the ask that made it, which is how the tool reads back its own answer")
	})
}
