package publicendpoint

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

func externalURLConfigMapKey() types.NamespacedName {
	return types.NamespacedName{
		Namespace: externalurl.Namespace,
		Name:      v1alpha1.WebdExternalURLConfigMap,
	}
}

// externalURLConfigMap returns the webd external-URL ConfigMap, or nil when the
// controller has written none. Nil is a real, distinguishable answer here: "no
// ConfigMap" and "a ConfigMap holding the wrong URL" are different failures and
// a test that could not tell them apart would pass on either.
func externalURLConfigMap(t *testing.T, c client.Client) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), externalURLConfigMapKey(), &cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err, "reading the webd external-URL ConfigMap")
	return &cm
}

// externalURL is the address webd reads for its trusted origin.
//
// It also pins that the SANDBOX key was written to the same value. webd serves
// artifact content from a second origin read out of sandbox-url, and a writer
// that updated only trusted-url would leave every artifact loading from a dead
// host while every assertion below still passed.
func externalURL(t *testing.T, c client.Client) string {
	t.Helper()
	cm := externalURLConfigMap(t, c)
	require.NotNil(t, cm, "the controller must have written ConfigMap %s", externalURLConfigMapKey())
	trusted := cm.Data[v1alpha1.WebdTrustedURLKey]
	assert.Equal(t, trusted, cm.Data[v1alpha1.WebdSandboxURLKey],
		"both webd origins must be written: sandbox-url is where artifact content loads from")
	return trusted
}

// editSpecBumpingGeneration is what makes the controller go back to the
// provider. An unchanged metadata.generation reuses the open tunnel and never
// reaches it at all, so a test that wanted a REOPEN and forgot the bump would
// silently exercise the reuse path instead.
//
// The fake client does not manage metadata.generation, so the bump a real
// apiserver applies on a spec change is applied here by hand and then read back
// — a bump that failed to persist would leave the test testing nothing.
func editSpecBumpingGeneration(t *testing.T, c client.Client, pe *v1alpha1.PublicEndpoint, newPort int32) {
	t.Helper()
	before := loadEndpoint(t, c, pe)
	edited := before.DeepCopy()
	edited.Spec.Target.Port = newPort
	edited.Generation = before.Generation + 1
	require.NoError(t, c.Update(context.Background(), edited), "persisting the spec edit")

	after := loadEndpoint(t, c, pe)
	require.NotEqual(t, before.Generation, after.Generation,
		"the bumped generation must have persisted; the reopen guard keys on it")
	require.Equal(t, newPort, after.Spec.Target.Port, "the spec edit must have persisted")
}

func readyReason(t *testing.T, pe *v1alpha1.PublicEndpoint) string {
	t.Helper()
	c := conditions.Find(pe.Status.Conditions, v1alpha1.PublicEndpointConditionReady)
	require.NotNil(t, c, "the Ready condition must be stamped")
	return c.Reason
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

// TestConfigMap_HoldsTheCreatorsLocalURLUntilTheTunnelIsReady covers the state
// this controller inherited from `oap install` and `oap desktop`: no tunnel yet,
// but webd's own browser surfaces still need an address they can build redirects
// and deep links from.
//
// The address must be the one the CR CARRIES, not one the controller picks. webd
// dispatches on an exact bare-host match, and the two flows that create these CRs
// disagree on both halves — `oap install` seeds "http://localhost:8080" while
// `oap desktop` binds 127.0.0.1 on a runtime-chosen port — so any constant is
// wrong for at least one of them, and a wrong host or port 404s every route at
// the address the user was told to open. The fixture's value matches neither
// historical guess, so a re-introduced default of either shape fails here.
func TestConfigMap_HoldsTheCreatorsLocalURLUntilTheTunnelIsReady(t *testing.T) {
	// No auth-token Secret in the fixture, so the endpoint settles on Pending.
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe)

	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	require.Equal(t, v1alpha1.PublicEndpointPhasePending, got.Status.Phase,
		"this test only means anything on the Pending path")
	require.Empty(t, got.Status.URL, "precondition: no public URL exists yet")

	assert.Equal(t, localURL, externalURL(t, c),
		"a cluster with no tunnel is still locally reachable; an empty URL breaks webd's redirects")
	assert.Equal(t, pe.Spec.LocalURL, externalURL(t, c),
		"the address must come from the CR, which is the only place the host-side address is known")
}

// TestConfigMap_TracksADifferentCreatorsLocalURL is the control for the test
// above. A controller that had kept a hardcoded loopback constant, or that read
// any source other than this CR's spec, passes that test only by coincidence if
// the constant happens to match the fixture — and fails the moment a second
// creator supplies a different address, which is exactly what `oap desktop`
// does relative to `oap install`.
func TestConfigMap_TracksADifferentCreatorsLocalURL(t *testing.T) {
	const desktopStyleURL = "http://127.0.0.1:19191"
	require.NotEqual(t, localURL, desktopStyleURL, "the two creators must disagree, or this proves nothing")

	pe := publicEndpointFixture(t)
	pe.Spec.LocalURL = desktopStyleURL
	r, c := newReconciler(t, pe)

	reconcileOnce(t, r, pe)

	require.Equal(t, v1alpha1.PublicEndpointPhasePending, loadEndpoint(t, c, pe).Status.Phase)
	assert.Equal(t, desktopStyleURL, externalURL(t, c),
		"a second creator's address must be published verbatim, host and port alike")
}

// TestReconcile_AnEmptyLocalURLIsRefusedRatherThanGuessed pins the runtime half
// of "required". The CRD marks spec.localURL required, so a live apiserver
// refuses an empty one at admission — but a CR created against Task 1's earlier
// schema has no value, and the controller must not then invent one.
func TestReconcile_AnEmptyLocalURLIsRefusedRatherThanGuessed(t *testing.T) {
	pe := publicEndpointFixture(t)
	pe.Spec.LocalURL = ""
	r, c := newReconciler(t, pe, authTokenSecret(t))

	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase)
	assert.Equal(t, v1alpha1.ReasonPublicEndpointLocalURLMissing, readyReason(t, &got))
	assert.Contains(t, conditionMessage(&got), "localURL",
		"the operator must be told which field to set")

	// The Secret DOES exist here, so nothing about the credential explains the
	// refusal — and no address was invented.
	assert.Nil(t, externalURLConfigMap(t, c),
		"a guessed local address 404s webd for every route; refuse instead of writing one")
	assert.Zero(t, activeFake.builtCount(), "no tunnel may be opened for a CR that cannot be published")
}

// TestConfigMap_IsNotWrittenForAnEndpointThatDoesNotTargetWebd closes a
// misdelivery path, not a cosmetic one. webd's external URL is what the channel
// planner seeds a new channel's external-base-url from, so an endpoint
// tunnelling some other Service — a dashboard, say — would have GitHub
// delivering webhook payloads, signed with that channel's secret, to a
// third-party host. Two endpoints would also flap the value against each other
// forever: one field owner plus ForceOwnership is last-writer-wins, never a
// surfaced conflict.
func TestConfigMap_IsNotWrittenForAnEndpointThatDoesNotTargetWebd(t *testing.T) {
	pe := publicEndpointFixture(t)
	pe.Spec.Target.Namespace = otherTargetNamespace
	pe.Spec.Target.Service = otherTargetService
	r, c := newReconciler(t, pe, authTokenSecret(t))

	reconcileOnce(t, r, pe)

	// The endpoint itself is entirely healthy — this is not a refusal of the
	// tunnel, only of the right to name webd's address.
	got := loadEndpoint(t, c, pe)
	require.Equal(t, v1alpha1.PublicEndpointPhaseReady, got.Status.Phase,
		"precondition: a non-webd endpoint still opens its tunnel normally")
	require.Equal(t, publicURL, got.Status.URL, "precondition: and still publishes its own URL")

	assert.Nil(t, externalURLConfigMap(t, c),
		"only an endpoint targeting webd may name the address every channel's external-base-url is seeded from")
}

// TestReconcile_AClusterKindThatRefusesTunnelsOpensNothing is the belt to the
// admission webhook's braces — and the half that can see a CR created BEFORE the
// policy existed, which admission never can.
//
// On a durable cluster `oap install` has already pointed webd at a real https://
// host, or seeded the ConfigMap EMPTY so credential links fail closed until it
// does. A tunnel here would overwrite either one, under ForceOwnership, on every
// reconcile.
func TestReconcile_AClusterKindThatRefusesTunnelsOpensNothing(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	// Re-point the reconciler at a kind whose policy is Never. Asserting the
	// policy rather than trusting the key keeps this test honest if the kind's
	// answer ever changes.
	r.ClusterKind = cloud.MustFor(cloud.KeyDefault)
	require.Equal(t, cloud.PublicEndpointNever, r.ClusterKind.InstallProfile().PublicEndpointPolicy(),
		"this test needs a kind that refuses tunnels")

	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase)
	assert.Equal(t, v1alpha1.ReasonPublicEndpointClusterKindRefusesTunnels, readyReason(t, &got))
	assert.Contains(t, conditionMessage(&got), cloud.KeyDefault,
		"the operator must be told which cluster kind refused")

	assert.Zero(t, activeFake.builtCount(),
		"no provider session may be opened on a kind with real ingress")
	assert.Nil(t, externalURLConfigMap(t, c),
		"overwriting a real external URL with a local one is the failure oap install goes out of its way to prevent")
}

// TestConfigMap_HoldsThePublicURLOnceReady is the fix this whole plan exists
// for: once a tunnel is open, the ConfigMap the channel planner seeds
// external-base-url from must carry the PUBLIC address, not a loopback one that
// GitHub refuses.
func TestConfigMap_HoldsThePublicURLOnceReady(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))

	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	require.Equal(t, v1alpha1.PublicEndpointPhaseReady, got.Status.Phase)

	assert.Equal(t, publicURL, externalURL(t, c))
	assert.Equal(t, got.Status.URL, externalURL(t, c),
		"the ConfigMap must carry the same address the CR publishes")
}

// TestConfigMap_FollowsTheReopenedTunnelsURL is the control for the test above:
// a writer that hardcoded the fixture URL, or that only ever wrote the FIRST
// address it saw, passes that one and fails this one. Only the pair proves the
// ConfigMap tracks the endpoint's CURRENT status.url.
func TestConfigMap_FollowsTheReopenedTunnelsURL(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)
	require.Equal(t, publicURL, externalURL(t, c), "precondition: the first tunnel's URL was published")

	// A spec edit reopens the tunnel, and the provider now answers with a
	// DIFFERENT address.
	prov.setURL(reopenedURL)
	editSpecBumpingGeneration(t, c, pe, 9001)
	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	require.Equal(t, reopenedURL, got.Status.URL, "precondition: the reopened tunnel republished status.url")

	assert.Equal(t, reopenedURL, externalURL(t, c),
		"the ConfigMap must follow the endpoint's current URL, not the first one it ever saw")
}

// TestConfigMap_KeepsItsLastGoodValueWhenTheTunnelFails pins the third state.
// Blanking the ConfigMap here would be worse than the stale value it replaces.
func TestConfigMap_KeepsItsLastGoodValueWhenTheTunnelFails(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)
	require.Equal(t, publicURL, externalURL(t, c), "precondition: a good URL was published")

	// Fail the tunnel. The spec edit is what sends the controller back to the
	// provider; without it the open tunnel is simply reused and the provider is
	// never consulted again.
	prov.failStart(errFakeStart)
	editSpecBumpingGeneration(t, c, pe, 9002)
	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	require.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase,
		"precondition: the endpoint must actually have failed")
	require.Empty(t, got.Status.URL, "precondition: a failed endpoint publishes no URL")

	assert.Equal(t, publicURL, externalURL(t, c),
		"a stale URL is bad; an empty one breaks every redirect and deep link webd serves")
}

// TestConfigMap_IsLeftAloneWhenNoTunnelEverOpened is the other half of the
// failure rule. "Keep the last good value" must mean "do not write", not "write
// something harmless": a controller that stamped a loopback address on every
// failure would pass the test above (its last good value happens to survive the
// reopen window) while silently overwriting whatever a cluster operator had
// configured on a cluster where no tunnel is wanted at all.
func TestConfigMap_IsLeftAloneWhenNoTunnelEverOpened(t *testing.T) {
	pe := publicEndpointFixture(t)
	pe.Spec.Provider = "nosuch"
	r, c := newReconciler(t, pe, authTokenSecret(t))

	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	require.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase)
	require.Equal(t, v1alpha1.ReasonPublicEndpointProviderUnknown, readyReason(t, &got),
		"precondition: Failed here must be the unknown provider, not a missing Secret")

	assert.Nil(t, externalURLConfigMap(t, c),
		"a Failed endpoint must write no external URL at all — the address belongs to whoever already owns it")
}

// TestConfigMap_IsNotBlankedWhileTheTunnelIsStillOpening covers the one window
// where "Pending" must NOT mean "write the loopback address".
//
// A reconcile that overlaps an in-flight open reports Pending with an empty
// status.url — the URL is seconds away and belongs to the reconcile that owns
// the reservation. A writer keyed on the phase alone would flap the ConfigMap
// from a live public URL to a loopback one and back on every reopen, and webd
// polls it, so every request in that window would land on a base-URL mismatch.
func TestConfigMap_IsNotBlankedWhileTheTunnelIsStillOpening(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)
	require.Equal(t, publicURL, externalURL(t, c), "precondition: a good URL was published")

	// Gate the NEXT open, then edit the spec so a reopen is required.
	entered, release := prov.gateStart()
	var releaseOnce sync.Once
	openGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(openGate)

	prov.setURL(reopenedURL)
	editSpecBumpingGeneration(t, c, pe, 9003)

	// Reconcile #1 parks inside the provider's Start, holding the reservation.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
		assert.NoError(t, err, "the parked reconcile must still succeed once released")
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first reconcile never reached the provider's Start")
	}

	// Reconcile #2 overlaps it and sees the `opening` reservation.
	secondDone := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the overlapping reconcile blocked inside the provider instead of deferring to the reservation")
	}

	got := loadEndpoint(t, c, pe)
	require.Equal(t, v1alpha1.ReasonPublicEndpointTunnelOpening, readyReason(t, &got),
		"precondition: the overlapping reconcile must have taken the still-opening path")
	require.Empty(t, got.Status.URL, "precondition: no URL is published while the tunnel is opening")

	assert.Equal(t, publicURL, externalURL(t, c),
		"the still-opening window must not replace a live public URL with a loopback one")

	// And once the open lands, the new URL is published normally.
	openGate()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the parked reconcile never returned after the gate opened")
	}
	assert.Equal(t, reopenedURL, externalURL(t, c),
		"the completed open must publish its URL")
}

// TestReconcile_AFailedConfigMapWriteIsReturnedWithoutARequeue pins both halves
// of how a failed apply is reported.
//
// controller-runtime IGNORES the result whenever the error is non-nil, and logs
// a warning saying so. Returning the Pending path's 30s re-check alongside the
// error would therefore spam that warning on a loop AND silently drop the timed
// re-check — so the error travels alone, and its rate-limited backoff is the
// retry.
func TestReconcile_AFailedConfigMapWriteIsReturnedWithoutARequeue(t *testing.T) {
	useFakeProvider(t)
	pe := publicEndpointFixture(t) // no Secret: Pending, whose result carries a RequeueAfter

	applyErr := errors.New("apply refused: fixture denies configmap writes")
	c := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(pe).
		WithStatusSubresource(&v1alpha1.PublicEndpoint{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				// Only the ConfigMap apply fails; the PublicEndpoint's own
				// patches must still work, or this test would be measuring a
				// different failure.
				if _, isCM := obj.(*corev1.ConfigMap); isCM {
					return applyErr
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &Reconciler{
		Client:       c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		ClusterKind:  cloud.MustFor(cloud.KeyLocal),
		Now:          func() time.Time { return fixedNow },
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})

	require.Error(t, err, "a ConfigMap write that failed must not be swallowed")
	assert.ErrorIs(t, err, applyErr, "the provider's own error must be wrapped, not replaced")
	assert.Zero(t, res.RequeueAfter,
		"a RequeueAfter returned beside a non-nil error is ignored by controller-runtime and logged as a warning")

	// The status write still happened: the CR must say what this reconcile
	// decided even though publishing the URL failed.
	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhasePending, got.Status.Phase,
		"the ConfigMap failure must not suppress the status write")
}

// TestConfigMap_IsGivenBackWhenTheEndpointIsDeleted is the delete-path half of
// this controller's ownership of webd's external URL, and the design's §5
// promise: "PublicEndpoint deleted → ConfigMap reverts to absent; dependent
// Channels go drifted, loudly."
//
// finalize used to stop the tunnel and remove the finalizer without touching
// the ConfigMap, so webd went on advertising — and every credential link, OAuth
// redirect_uri and artifact deep-link went on carrying — a public address that
// forwards nothing. On `desktop` the host-side writer takes the keys back at
// the next boot; on `local` nothing does, ever. That is this feature's own
// root-cause bug, re-created on the way out.
func TestConfigMap_IsGivenBackWhenTheEndpointIsDeleted(t *testing.T) {
	ctx := context.Background()
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))

	reconcileOnce(t, r, pe)
	require.Equal(t, publicURL, externalURL(t, c), "precondition: the tunnel's address is published")

	require.NoError(t, c.Delete(ctx, pe.DeepCopy()))
	reconcileOnce(t, r, pe)

	cm := externalURLConfigMap(t, c)
	require.NotNil(t, cm, "the ConfigMap itself is not this controller's to delete — only the two keys it owns")
	assert.Empty(t, cm.Data[v1alpha1.WebdTrustedURLKey],
		"a tunnel that is gone must not go on being advertised")
	assert.Empty(t, cm.Data[v1alpha1.WebdSandboxURLKey],
		"and the sandbox origin is the same address, released the same way")

	var got v1alpha1.PublicEndpoint
	err := c.Get(ctx, key(pe), &got)
	require.Error(t, err, "the object must still be released")
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound after the finalizer was removed, got %v", err)
}

// TestConfigMap_ADeletedEndpointThatNeverOwnedTheURLLeavesItAlone is the
// control on the release's scope, and the scenario is the one where it bites:
// two endpoints, one tunnelling webd and one tunnelling something else, and the
// OTHER one is deleted.
//
// The webd endpoint's tunnel is still up and its address is still published, so
// a release that ran for every deleted endpoint would take a live URL out from
// under it — the write gate's mirror image, and the same reasoning
// (TestConfigMap_IsNotWrittenForAnEndpointThatDoesNotTargetWebd).
//
// It has to be THIS controller's field manager that owns the keys for the
// control to be able to fail: server-side apply only removes what the applying
// manager held, so a value written by anything else would survive a release
// that had no gate at all, and the test would pass on a broken controller.
func TestConfigMap_ADeletedEndpointThatNeverOwnedTheURLLeavesItAlone(t *testing.T) {
	ctx := context.Background()

	webdEndpoint := publicEndpointFixture(t)
	other := publicEndpointFixture(t)
	other.Name = "demo-dashboard-endpoint"
	other.Spec.Target.Namespace = otherTargetNamespace
	other.Spec.Target.Service = otherTargetService

	r, c := newReconciler(t, webdEndpoint, other, authTokenSecret(t))

	// The webd endpoint publishes first, so the two keys are held under this
	// controller's own field manager.
	reconcileOnce(t, r, webdEndpoint)
	require.Equal(t, publicURL, externalURL(t, c), "precondition: webd's address is published, and ours")

	require.NoError(t, c.Delete(ctx, other.DeepCopy()))
	reconcileOnce(t, r, other)

	assert.Equal(t, publicURL, externalURL(t, c),
		"an endpoint that never named this address does not get to blank it on the way out — "+
			"the webd endpoint's tunnel is still up and still published")
}

// TestFinalize_HoldsTheObjectWhileTheReleaseFailsThenLetsItGo pins both halves
// of the bound, because each alone is a failure mode:
//
//   - Let go on the first error and one apiserver blip leaves a dead public URL
//     advertised for the life of the cluster — the exact thing the release
//     exists to prevent.
//   - Retry forever and a permanently unwritable ConfigMap makes the CR
//     undeletable, wedging every namespace teardown behind it.
//
// The clock is what moves between the two passes, so this measures the bound
// and not a timeout.
func TestFinalize_HoldsTheObjectWhileTheReleaseFailsThenLetsItGo(t *testing.T) {
	useFakeProvider(t)
	ctx := context.Background()
	pe := publicEndpointFixture(t)

	applyErr := errors.New("apply refused: fixture denies configmap writes")
	c := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(pe).
		WithStatusSubresource(&v1alpha1.PublicEndpoint{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, isCM := obj.(*corev1.ConfigMap); isCM {
					return applyErr
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	// now is a var the reconciler reads through, so the two passes below differ
	// only in how long the deletion has been pending.
	now := fixedNow
	r := &Reconciler{
		Client:       c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		ClusterKind:  cloud.MustFor(cloud.KeyLocal),
		Now:          func() time.Time { return now },
	}

	require.NoError(t, c.Delete(ctx, pe.DeepCopy()))
	deletedAt := loadEndpoint(t, c, pe).DeletionTimestamp
	require.NotNil(t, deletedAt, "precondition: the finalizer must have held the object")

	// Inside the grace window: loud, retried, and the object is NOT released.
	now = deletedAt.Add(time.Second)
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key(pe)})
	require.Error(t, err, "a release this reconcile could not do must not be swallowed")
	assert.ErrorIs(t, err, applyErr, "the underlying failure must be wrapped, not replaced")
	got := loadEndpoint(t, c, pe)
	assert.True(t, controllerutil.ContainsFinalizer(&got, v1alpha1.FinalizerPublicEndpoint),
		"the object is held while the release is still worth retrying")

	// Past it: the object is let go anyway, because an undeletable CR is worse
	// than a stale value somebody can correct.
	now = deletedAt.Add(externalURLReleaseGrace + time.Second)
	_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key(pe)})
	require.NoError(t, err, "past the bound the deletion completes rather than wedging")
	err = c.Get(ctx, key(pe), &got)
	require.Error(t, err, "the object must be released once the bound has passed")
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
}
