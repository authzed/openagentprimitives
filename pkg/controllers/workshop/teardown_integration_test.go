//go:build integration

package workshop_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// newBuildSession creates an AgentSession that satisfies the CRD's
// structural schema (Class + Prompt are required) — mirrors
// provision_integration_test.go's TestProvision_RealAPIServerStateAndIdempotency
// so every teardown test starts from the exact fixture shape provisioning
// itself is tested against.
func newBuildSession(t *testing.T, ctx context.Context, env *testenv.Env, name string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	sess := builderSession(name, "placeholder-overwritten-by-apiserver")
	sess.Spec = spiceboxv1alpha1.AgentSessionSpec{
		Class:  "workshop-test-class",
		Prompt: spiceboxv1alpha1.PromptSource{Inline: "build something"},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	return sess
}

// reconcileUntilGone drives Reconcile against a real apiserver until the
// Workshop CR is gone (the terminal signal that teardown finished: the
// finalizer was removed and Kubernetes deleted the object) or an error
// surfaces. A bounded loop, like reconcileUntilReady, tolerates however many
// reconciles teardown's own steps take.
func reconcileUntilGone(t *testing.T, ctx context.Context, r *workshop.Reconciler, key types.NamespacedName) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			require.NoError(t, err, "teardown reconcile step %d", i)
		}
		var got spiceboxv1alpha1.Workshop
		err := r.Client.Get(ctx, key, &got)
		if apierrors.IsNotFound(err) {
			return
		}
		require.NoError(t, err, "get Workshop after teardown reconcile step %d", i)
	}
	t.Fatalf("Workshop %s was not fully torn down within 5 reconciles", key)
}

// TestTeardown_ReversesEveryLayer is the fully-provisioned round trip: every
// layer Reconcile stood up — the namespace, the toolwriter CRB, a
// later-plan-authored cluster-scoped tool CR, the SpiceDB tuple, the bearer
// registration — must be gone before the Workshop CR itself is, and the
// finalizer must never linger once it is.
func TestTeardown_ReversesEveryLayer(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := newBuildSession(t, ctx, env, "wsbuild-teardown")
	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	reg := tokens.NewRegistry()
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    reg,
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	reconcileUntilReady(t, ctx, r, key)

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))
	nsName := got.Status.Namespace
	require.NotEmpty(t, nsName)

	// A later-plan authored tool CR, cluster-scoped and labeled for this
	// workshop — the one standing thing owner-ref GC could never reach on its
	// own (SpiceboxToolspec cannot carry a namespaced owner ref).
	probe := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nsName + "-probe",
			Labels: map[string]string{spiceboxv1alpha1.LabelWorkshopNamespace: nsName},
		},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             nsName + "-probe",
			Version:          "1",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "echo", Revision: "2026-04-25"},
			AllowSubcommands: []string{""},
		},
	}
	require.NoError(t, env.Client.Create(ctx, probe), "create labeled SpiceboxToolspec probe")

	require.NoError(t, env.Client.Delete(ctx, &got), "delete Workshop")
	reconcileUntilGone(t, ctx, r, key)

	// Namespace: gone, or at least marked for deletion — envtest runs no
	// namespace-GC controller to finish an async delete, but the apiserver
	// itself stamps DeletionTimestamp/Terminating synchronously on Delete.
	// Teardown deletes the namespace; namespaced objects (the browser Role/
	// RoleBinding, when a reconciler provisions them) go with it via the
	// namespace controller, which envtest does not run. This test's
	// Reconciler sets no BrowserServiceAccount, so it provisions none — the
	// browser Role/RoleBinding's existence and shape are asserted at
	// provision time instead (TestProvision_RealAPIServerStateAndIdempotency).
	var ns corev1.Namespace
	if err := env.Client.Get(ctx, types.NamespacedName{Name: nsName}, &ns); err == nil {
		assert.NotNil(t, ns.DeletionTimestamp, "leftover workshop namespace must at least be marked for deletion")
	} else {
		assert.True(t, apierrors.IsNotFound(err), "unexpected error getting workshop namespace: %v", err)
	}

	var crb rbacv1.ClusterRoleBinding
	err := env.Client.Get(ctx, types.NamespacedName{Name: workshop.WorkshopToolwriterCRBName(ws.Namespace, sess.Name)}, &crb)
	assert.True(t, apierrors.IsNotFound(err), "toolwriter ClusterRoleBinding must be gone: %v", err)

	var readerCRB rbacv1.ClusterRoleBinding
	err = env.Client.Get(ctx, types.NamespacedName{Name: workshop.WorkshopReaderCRBName(ws.Namespace, sess.Name)}, &readerCRB)
	assert.True(t, apierrors.IsNotFound(err), "reader ClusterRoleBinding must be gone: %v", err)

	var gotProbe spiceboxv1alpha1.SpiceboxToolspec
	err = env.Client.Get(ctx, types.NamespacedName{Name: probe.Name}, &gotProbe)
	assert.True(t, apierrors.IsNotFound(err), "the labeled SpiceboxToolspec must be gone: %v", err)

	assert.Equal(t, []string{nsName}, ft.deleted, "the tuple must be deleted for exactly this workshop's namespace id")
	assert.False(t, reg.Registered(memory.NamespacedName{Namespace: ws.Namespace, Name: spiceboxv1alpha1.WorkshopName(sess.Name)}),
		"the bearer registration must be revoked")
}

// TestTeardown_ForeignNamespaceIsNotDeleted is the C1 refusing-direction test
// for teardown's label guard: when status.Namespace names a namespace that
// EXISTS but carries a DIFFERENT session's labels (a namespace this workshop
// does not own — the shape a name-collision or a future ordering regression
// could leave behind), teardown must NOT delete it, and must STILL release the
// finalizer (nothing of ours stands in a namespace that is not ours).
func TestTeardown_ForeignNamespaceIsNotDeleted(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	// A namespace this workshop does NOT own: it exists and carries a DIFFERENT
	// session's workshop labels. A teardown that deleted status.Namespace by
	// name with no label check would destroy it (cross-namespace data loss).
	foreignNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-foreign-teardown",
			Labels: map[string]string{
				spiceboxv1alpha1.LabelWorkshopSessionNamespace: "some-other-namespace",
				spiceboxv1alpha1.LabelWorkshopSessionName:      "some-other-session",
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, foreignNS), "create the foreign namespace")

	sess := newBuildSession(t, ctx, env, "wsbuild-teardown-foreign")
	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    tokens.NewRegistry(),
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}

	// Add the finalizer, then anchor status.Namespace at the FOREIGN namespace
	// directly — simulating a status that (via a hypothetical ordering
	// regression) points at a namespace this workshop does not own.
	primeFinalizer(t, r, ws)
	var anchored spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &anchored))
	anchored.Status.Namespace = foreignNS.Name
	anchored.Status.Phase = spiceboxv1alpha1.WorkshopPhaseProvisioning
	require.NoError(t, env.Client.Status().Update(ctx, &anchored), "point status.Namespace at the foreign namespace")

	require.NoError(t, env.Client.Delete(ctx, &anchored), "delete Workshop")
	// reconcileUntilGone proves the finalizer WAS released: the CR is gone.
	reconcileUntilGone(t, ctx, r, key)

	// The foreign namespace must be completely untouched — never deleted, never
	// even marked for deletion.
	var stillThere corev1.Namespace
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: foreignNS.Name}, &stillThere),
		"the foreign namespace must still exist — teardown must never delete a namespace this workshop does not own")
	assert.Nil(t, stillThere.DeletionTimestamp, "the foreign namespace must not be marked for deletion")
}

// TestTeardown_TupleDeleteErrorKeepsFinalizer is the fail-closed half of the
// security property: a tuple deletion that cannot be confirmed must never be
// traded for a released finalizer, because that finalizer is the ONLY thing
// blocking the workshop namespace, RBAC and Secret from disappearing while a
// standing SpiceDB relationship survives them.
func TestTeardown_TupleDeleteErrorKeepsFinalizer(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := newBuildSession(t, ctx, env, "wsbuild-teardown-err")
	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    tokens.NewRegistry(),
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	reconcileUntilReady(t, ctx, r, key)

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))

	// From here on, DeleteWorkshopRelationships always errors — as if SpiceDB
	// were unreachable at the exact moment teardown needs to confirm the
	// tuple is gone.
	ft.err = fmt.Errorf("spicedb: connection refused")

	require.NoError(t, env.Client.Delete(ctx, &got), "delete Workshop")
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.Error(t, err, "teardown must surface an unconfirmed tuple deletion as an error")
	assert.Contains(t, err.Error(), "spicedb: connection refused")

	var stillThere spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &stillThere), "the Workshop must still exist — the finalizer was not released")
	assert.Contains(t, stillThere.Finalizers, spiceboxv1alpha1.FinalizerWorkshop,
		"the finalizer must remain pinned while the tuple's deletion is unconfirmed")
}

// TestSweepExpiry_ExpiresReadyWorkshopPastMaxAge covers the max-age sweeper:
// a Ready workshop whose clock has run past spec.limits.maxAge is expired —
// Phase flips, the person who started it is (best-effort) told, and the CR
// is deleted — in the SAME reconcile that notices the overrun, with the
// actual teardown left to the reconciles that follow.
func TestSweepExpiry_ExpiresReadyWorkshopPastMaxAge(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := newBuildSession(t, ctx, env, "wsbuild-sweep")
	ws := sanctionedWorkshop(sess)
	ws.Spec.Limits.MaxAge = metav1.Duration{Duration: time.Nanosecond}
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	type notice struct{ ns, name, requester, body string }
	var notices []notice
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    tokens.NewRegistry(),
		ExpiredNoticePublish: func(_ context.Context, ns, name, requester, body string) error {
			notices = append(notices, notice{ns, name, requester, body})
			return nil
		},
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	reconcileUntilReady(t, ctx, r, key)

	var readyWs spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &readyWs))
	nsName := readyWs.Status.Namespace
	require.NotEmpty(t, nsName)

	// Push the clock far past provisionedAt so even a 1ns MaxAge is exceeded,
	// and reconcile exactly once.
	r.Now = func() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err, "the expiry reconcile must succeed")

	var expired spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &expired), "the Workshop must still exist (finalizer pending) right after the expiry reconcile")
	assert.Equal(t, spiceboxv1alpha1.WorkshopPhaseExpired, expired.Status.Phase)
	assert.NotNil(t, expired.DeletionTimestamp, "the expiry reconcile must Delete the CR")

	require.Len(t, notices, 1, "exactly one expiry notice must be published")
	assert.Equal(t, sess.Namespace, notices[0].ns)
	assert.Equal(t, sess.Name, notices[0].name)
	assert.Equal(t, "user:"+ws.Spec.StarterCanonical, notices[0].requester)
	assert.Equal(t, "This build space expired and was cleaned up. Your draft is safe — start a new build to continue from it.", notices[0].body)

	// Subsequent reconciles run the teardown, same as a manual delete.
	reconcileUntilGone(t, ctx, r, key)
	assert.Equal(t, []string{nsName}, ft.deleted)
}

// TestTeardown_SessionGoneStillReversesEveryLayer is the Fix-round-1 Critical
// regression test: on the ORDINARY owner-ref GC cascade, the AgentSession is
// removed from etcd before the Workshop it owns is reaped, so a teardown that
// depended on re-Getting the session to learn the namespace id would 404 —
// exactly the gap that let a live tuple, a live namespace and a live CRB
// outlive their workshop with no log. status.Namespace is now the durable
// anchor teardown reads instead, so deleting the session FIRST (matching what
// the cascade actually does) must not change what gets reversed at all.
func TestTeardown_SessionGoneStillReversesEveryLayer(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := newBuildSession(t, ctx, env, "wsbuild-teardown-sessgone")
	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	reg := tokens.NewRegistry()
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    reg,
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	reconcileUntilReady(t, ctx, r, key)

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))
	nsName := got.Status.Namespace
	require.NotEmpty(t, nsName)

	// Simulate the ordinary owner-ref GC cascade: the session is reaped
	// BEFORE the Workshop it owns. Deleting it explicitly (rather than
	// relying on envtest to run GC, which it doesn't) is what makes
	// teardown's world match the real cascade: a session Get from inside
	// teardown must 404 from here on.
	require.NoError(t, env.Client.Delete(ctx, sess), "delete AgentSession")
	var goneSess spiceboxv1alpha1.AgentSession
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &goneSess)
	require.True(t, apierrors.IsNotFound(err), "the session must actually be gone before this test proves anything: %v", err)

	require.NoError(t, env.Client.Delete(ctx, &got), "delete Workshop")
	reconcileUntilGone(t, ctx, r, key)

	var ns corev1.Namespace
	if err := env.Client.Get(ctx, types.NamespacedName{Name: nsName}, &ns); err == nil {
		assert.NotNil(t, ns.DeletionTimestamp, "leftover workshop namespace must at least be marked for deletion")
	} else {
		assert.True(t, apierrors.IsNotFound(err), "unexpected error getting workshop namespace: %v", err)
	}

	var crb rbacv1.ClusterRoleBinding
	err = env.Client.Get(ctx, types.NamespacedName{Name: workshop.WorkshopToolwriterCRBName(ws.Namespace, sess.Name)}, &crb)
	assert.True(t, apierrors.IsNotFound(err), "toolwriter ClusterRoleBinding must be gone: %v", err)

	var readerCRB rbacv1.ClusterRoleBinding
	err = env.Client.Get(ctx, types.NamespacedName{Name: workshop.WorkshopReaderCRBName(ws.Namespace, sess.Name)}, &readerCRB)
	assert.True(t, apierrors.IsNotFound(err), "reader ClusterRoleBinding must be gone: %v", err)

	assert.Equal(t, []string{nsName}, ft.deleted,
		"the tuple must be deleted even though the session was already gone by the time teardown ran")
	assert.False(t, reg.Registered(memory.NamespacedName{Namespace: ws.Namespace, Name: spiceboxv1alpha1.WorkshopName(sess.Name)}),
		"the bearer registration must be revoked")
}

// TestTeardown_NeverProvisionedReleasesFinalizerWithoutTouchingAnything covers
// the other half of the anchor fix: a Workshop whose finalizer was added but
// which never got far enough to persist status.Namespace (provisioning was
// never even run here) must release cleanly — no tuple-deletion attempt, no
// namespace-delete attempt — because the anchor is now written before any
// external state can exist, so an empty status.Namespace is a reliable "there
// is nothing to reverse" rather than an ambiguous one.
func TestTeardown_NeverProvisionedReleasesFinalizerWithoutTouchingAnything(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	// Deliberately not created: the whole point of this test is a Workshop
	// that never ran a provisioning reconcile, so nothing about the session
	// needs to exist for the assertions below to hold.
	sess := builderSession("wsbuild-teardown-never", "placeholder-never-used")
	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    tokens.NewRegistry(),
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}
	primeFinalizer(t, r, ws) // adds the finalizer and returns — never reaches the anchor write

	var got spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &got))
	require.Empty(t, got.Status.Namespace, "this test's premise is a workshop that never recorded a namespace")

	require.NoError(t, env.Client.Delete(ctx, &got), "delete Workshop")
	reconcileUntilGone(t, ctx, r, key)

	assert.Empty(t, ft.deleted, "nothing was ever provisioned, so no tuple deletion should have been attempted")
}

// TestTeardown_TupleWrittenBeforeReady_SessionGone_StillDeletesTuple is the
// precise reproduction of the Fix-round-1 Critical: a workshop that WROTE the
// SpiceDB tuple (Reconcile's tuple step) but never reached Ready — a nil
// Tokens registry fails it closed at the very next step, the same crash
// window a real restart between the tuple write and the end-of-provisioning
// persist would leave — combined with the session ALSO being gone by the
// time teardown runs (the ordinary GC cascade). Before the durable-anchor fix
// this exact combination left status.Namespace empty AND the session Get
// 404ing, so nsName stayed "" and teardown silently skipped the tuple
// deletion entirely — the orphan the Critical finding named. The anchor (set
// before the tuple can ever be written) closes it: status.Namespace is
// already durably recorded by the time this test deletes the session, so
// teardown finds the id with no session lookup at all.
func TestTeardown_TupleWrittenBeforeReady_SessionGone_StillDeletesTuple(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	sess := newBuildSession(t, ctx, env, "wsbuild-teardown-partial")
	ws := sanctionedWorkshop(sess)
	require.NoError(t, env.Client.Create(ctx, ws), "create Workshop")

	ft := &fakeTuples{}
	r := &workshop.Reconciler{
		Client:    env.Client,
		APIReader: env.Client,
		Tuples:    ft,
		Tokens:    nil, // fails Reconcile closed at TokensReady, AFTER the tuple write
	}
	key := types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}

	primeFinalizer(t, r, ws)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.Error(t, err, "provisioning must fail closed at TokensReady with a nil Tokens registry")

	var stuck spiceboxv1alpha1.Workshop
	require.NoError(t, env.Client.Get(ctx, key, &stuck))
	require.NotEqual(t, spiceboxv1alpha1.WorkshopPhaseReady, stuck.Status.Phase,
		"this test's premise is a workshop that never reached Ready")
	require.NotEmpty(t, stuck.Status.Namespace,
		"the durable anchor must have persisted status.Namespace before the tuple write, even though Ready was never reached")
	require.Len(t, ft.ensured, 1, "the tuple write must have happened before the TokensReady failure")
	nsName := stuck.Status.Namespace

	// The ordinary GC cascade: the session disappears before this Workshop is
	// reaped.
	require.NoError(t, env.Client.Delete(ctx, sess), "delete AgentSession")
	var goneSess spiceboxv1alpha1.AgentSession
	getErr := env.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}, &goneSess)
	require.True(t, apierrors.IsNotFound(getErr), "the session must actually be gone before this test proves anything: %v", getErr)

	require.NoError(t, env.Client.Delete(ctx, &stuck), "delete Workshop")
	reconcileUntilGone(t, ctx, r, key)

	assert.Equal(t, []string{nsName}, ft.deleted,
		"the tuple written before the crash must still be deleted, even though the workshop never reached Ready and the session is already gone")
}
