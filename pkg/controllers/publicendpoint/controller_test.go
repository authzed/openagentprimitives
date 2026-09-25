package publicendpoint

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	// The two cluster kinds these tests reconcile as. Blank imports because the
	// kinds are resolved through cloud.MustFor by key, never referenced by type
	// — `local` permits a tunnel (policy Always), `unmanaged` refuses one
	// (policy Never), and one of each is what makes the refusal testable.
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel/registry"
	"github.com/authzed/openagentprimitives/pkg/web/localtunnel/stub"
)

// Fixture values. Every one is DISTINCT from every other: an assertion that
// finds "ngrok-authtoken" in a condition message can only have found the
// Secret's name, an assertion on the published URL can only have found the
// provider's answer, and so on. Two fields sharing a value is how a test
// passes while examining the wrong thing.
const (
	endpointName    = "demo-endpoint"
	secretNamespace = "demo-system"
	secretName      = "ngrok-authtoken"
	secretKey       = "token"
	authTokenValue  = "fixture-token-value"
	// The target is webd's REAL Service coordinates, not a made-up pair: the
	// controller writes webd's external-URL ConfigMap only for an endpoint that
	// targets webd, so a fixture pointed anywhere else exercises the refusal
	// instead of the path most of these tests name.
	// TestConfigMap_IsNotWrittenForAnEndpointThatDoesNotTargetWebd owns that
	// case and uses otherTarget* below.
	targetNamespace = cloud.WebdServiceNamespace
	targetService   = cloud.WebdServiceName
	// Port stays distinct from webd's own 8080 on purpose: the webd gate keys
	// on namespace+service ONLY, and a fixture whose port also matched could
	// not tell a gate that ignores the port from one that compares it.
	targetPort = int32(8443)

	// otherTarget* is a Service that is emphatically NOT webd, for the refusal
	// test. Distinct in BOTH components, so a gate comparing only one of them
	// still refuses and the test cannot pass by half-checking.
	otherTargetNamespace = "demo-observability"
	otherTargetService   = "demo-dashboard"

	// localURL is the host-side address the CR's creator supplies. It matches
	// NEITHER of the two loopback addresses this controller used to guess
	// ("http://localhost:8080" from oap install, "http://127.0.0.1:<port>" from
	// oap desktop), so a re-introduced hardcoded default of either shape fails
	// every assertion that names it.
	localURL       = "http://localhost:17080"
	reservedDomain = "pinned.example.invalid"
	publicURL      = "https://opened-by-the-fake.example.invalid"
	reopenedURL    = "https://reopened-by-the-fake.example.invalid"
)

// errFakeStart is what the fake provider refuses with in the failure test. Its
// text appears nowhere else, so finding it in a condition message proves the
// provider's own error was propagated rather than some generic wording.
var errFakeStart = errors.New("provider refused: quota exhausted")

// -----------------------------------------------------------------------
// The fake tunnel provider these tests resolve through the registry.
// -----------------------------------------------------------------------

// testProviderName is the provider name every fixture here uses.
//
// NO TEST IN THIS PACKAGE CAN REACH THE NETWORK. The only provider that talks
// to the Internet is pkg/web/localtunnel/ngrok, and it is imported by neither
// controller.go nor this file — so it is not linked into this test binary, and
// registry.Get("ngrok") cannot resolve here at all.
// TestNoNetworkTunnelProviderIsLinked asserts exactly that, so the guarantee
// survives someone adding an import later.
const testProviderName = "faketunnel"

// activeFake is the fake the registered factory builds from. It is
// package-level because registry.Factory is a bare func with nowhere to hang a
// per-test handle. Tests install one via useFakeProvider and therefore MUST
// NOT run in parallel with each other.
var activeFake *fakeProvider

// fakeProvider records every Tunnel the controller asked it to build and the
// Options it was built with, so a test can prove exactly how many tunnels were
// opened and what credential reached the provider.
type fakeProvider struct {
	mu       sync.Mutex
	url      string
	startErr error
	built    []*fakeTunnel
	opts     []registry.Options
	entered  chan struct{}
	release  <-chan struct{}
}

// fakeTunnel is pkg/web/localtunnel/stub's double plus the two things these
// tests need that it does not offer: the context Start was handed (so the
// tunnel's LIFETIME can be asserted — the reconcile context is request-scoped
// and would be dead by the time anyone looked) and an optional gate (so an
// open can be held mid-flight while a second reconcile overlaps it).
// Everything else — Started, Stopped, LocalAddr, the URL/StartErr behaviour —
// is the stub's, promoted through the embedded pointer.
type fakeTunnel struct {
	*stub.Tunnel

	mu       sync.Mutex
	startCtx context.Context //nolint:containedctx // the point: assert the ctx Start was given
	entered  chan struct{}
	release  <-chan struct{}
}

func (f *fakeTunnel) Start(ctx context.Context, localAddr string) (string, error) {
	f.mu.Lock()
	f.startCtx = ctx
	entered, release := f.entered, f.release
	f.mu.Unlock()

	if entered != nil {
		// Buffered + non-blocking: a SECOND tunnel entering Start (the bug
		// these gates exist to catch) must not panic or deadlock the signal.
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.Tunnel.Start(ctx, localAddr)
}

// StartCtx is the context Start was handed; nil before Start.
func (f *fakeTunnel) StartCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCtx
}

func (p *fakeProvider) build(o registry.Options) localtunnel.Tunnel {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := &fakeTunnel{
		Tunnel:  &stub.Tunnel{URL: p.url, StartErr: p.startErr},
		entered: p.entered,
		release: p.release,
	}
	p.built = append(p.built, t)
	p.opts = append(p.opts, o)
	return t
}

// gateStart makes every subsequently-built tunnel park inside Start until the
// returned release channel is closed, signalling entry on `entered`. It is how
// a test observes the window in which the `opening` reservation is the only
// thing standing between two reconciles and two provider sessions.
func (p *fakeProvider) gateStart() (entered <-chan struct{}, release chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := make(chan struct{}, 1)
	rel := make(chan struct{})
	p.entered, p.release = e, rel
	return e, rel
}

// builtCount is the number of tunnels the controller has asked for. The
// central invariant of this controller is that it stays at 1 across repeated
// reconciles of an unchanged spec.
func (p *fakeProvider) builtCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.built)
}

func (p *fakeProvider) tunnelAt(t *testing.T, i int) *fakeTunnel {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Greater(t, len(p.built), i, "provider must have been asked for at least %d tunnel(s)", i+1)
	return p.built[i]
}

func (p *fakeProvider) optionsAt(t *testing.T, i int) registry.Options {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Greater(t, len(p.opts), i, "provider must have been asked for at least %d tunnel(s)", i+1)
	return p.opts[i]
}

// setURL changes what subsequently-built tunnels return from Start, so a
// reopen can be told apart from a reuse by the URL alone.
func (p *fakeProvider) setURL(u string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.url = u
}

// failStart makes every subsequently-built tunnel refuse to open.
func (p *fakeProvider) failStart(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startErr = err
}

func init() {
	registry.Register(testProviderName, func(o registry.Options) localtunnel.Tunnel {
		if activeFake == nil {
			// Panic rather than return a nil interface the controller would
			// then call Start on: reaching the registry without a fake
			// installed is a test bug, and a nil return would surface as an
			// unrelated panic deep inside the controller.
			panic("publicendpoint tests: no fake provider installed — call useFakeProvider(t)")
		}
		return activeFake.build(o)
	})
}

func useFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{url: publicURL}
	activeFake = p
	t.Cleanup(func() { activeFake = nil })
	return p
}

// -----------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func key(pe *v1alpha1.PublicEndpoint) types.NamespacedName {
	return types.NamespacedName{Name: pe.Name} // PublicEndpoint is cluster-scoped
}

// publicEndpointFixture is a well-formed PublicEndpoint whose authTokenRef
// names a Secret the caller may or may not create. The finalizer is
// pre-stamped so a single Reconcile reaches the tunnel logic rather than
// spending its first pass adding it — TestReconcile_AddsFinalizerBefore…
// covers that pass on its own.
func publicEndpointFixture(t *testing.T) *v1alpha1.PublicEndpoint {
	t.Helper()
	return &v1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:       endpointName,
			Generation: 1,
			Finalizers: []string{v1alpha1.FinalizerPublicEndpoint},
		},
		Spec: v1alpha1.PublicEndpointSpec{
			Target: v1alpha1.PublicEndpointTarget{
				Namespace: targetNamespace,
				Service:   targetService,
				Port:      targetPort,
			},
			Provider:     testProviderName,
			AuthTokenRef: v1alpha1.ClusterSecretKeyRef{Namespace: secretNamespace, Name: secretName, Key: secretKey},
			LocalURL:     localURL,
			// ReservedDomain is deliberately EMPTY: a non-empty value is
			// refused fail-closed while no provider honors it, so setting it
			// here would make every other test in this file exercise the
			// refusal instead of the path it names.
			// TestReconcile_ReservedDomainIsRefusedUntilAProviderHonorsIt owns
			// that case.
		},
	}
}

// authTokenSecret is the Secret publicEndpointFixture's authTokenRef points
// at, pre-stamped with the adoption label the way a live cluster's Secret
// looks once the operator has adopted it.
func authTokenSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: secretNamespace, Name: secretName},
		Data:       map[string][]byte{secretKey: []byte(authTokenValue)},
	}
	adoptguard.WithAdoptedLabel(s)
	return s
}

// fixedNow is the clock the reconciler stamps status.observedAt from, so a
// test can tell "the controller stamped it" from "something else did".
var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// newReconciler builds a fake-client-backed Reconciler over objs, with a fake
// tunnel provider installed for the duration of the test.
func newReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	r, c, _ := newReconcilerCountingStatusWrites(t, objs...)
	return r, c
}

// newReconcilerCountingStatusWrites is newReconciler plus a counter of status
// subresource writes, for the no-churn assertions.
func newReconcilerCountingStatusWrites(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *atomic.Int64) {
	t.Helper()
	useFakeProvider(t)

	var statusWrites atomic.Int64

	c := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.PublicEndpoint{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if sub == "status" {
					statusWrites.Add(1)
				}
				return cl.Status().Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if sub == "status" {
					statusWrites.Add(1)
				}
				return cl.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	// Warn mode, empty allowlist: the fake client is both the live reader and
	// the writer, exactly as pkg/controllers/clusteridentityprovider's tests
	// wire it.
	r := &Reconciler{
		Client:       c,
		SecretReader: adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		// `local` is the kind whose PublicEndpointPolicy permits a tunnel, so
		// every test that is not ABOUT the refusal reaches the tunnel logic.
		// TestReconcile_AClusterKindThatRefusesTunnelsOpensNothing wires the
		// other kind.
		ClusterKind: cloud.MustFor(cloud.KeyLocal),
		Now:         func() time.Time { return fixedNow },
	}
	return r, c, &statusWrites
}

// conditionMessage is the message on the Ready condition, looked up by its
// exact type so a controller that stamped some other condition type reports an
// empty message here rather than accidentally matching.
func conditionMessage(pe *v1alpha1.PublicEndpoint) string {
	c := conditions.Find(pe.Status.Conditions, v1alpha1.PublicEndpointConditionReady)
	if c == nil {
		return ""
	}
	return c.Message
}

func reconcileOnce(t *testing.T, r *Reconciler, pe *v1alpha1.PublicEndpoint) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
	require.NoError(t, err, "Reconcile must not return a hard error")
	return res
}

func loadEndpoint(t *testing.T, c client.Client, pe *v1alpha1.PublicEndpoint) v1alpha1.PublicEndpoint {
	t.Helper()
	var got v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(context.Background(), key(pe), &got), "Get after Reconcile")
	return got
}

// -----------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------

func TestReconcile_MissingAuthTokenSecretIsPendingNotFailed(t *testing.T) {
	pe := publicEndpointFixture(t) // authTokenRef names a Secret that does not exist
	r, c := newReconciler(t, pe)

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
	require.NoError(t, err, "a missing prerequisite is a state to report, not a reconcile error to retry forever")

	var got v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(context.Background(), key(pe), &got))
	assert.Equal(t, v1alpha1.PublicEndpointPhasePending, got.Status.Phase)
	assert.Empty(t, got.Status.URL, "no URL may be published before one exists")
	assert.Contains(t, conditionMessage(&got), "ngrok-authtoken",
		"the operator must be told which Secret is missing")

	// And nothing was opened: a missing credential must never reach a provider.
	assert.Zero(t, activeFake.builtCount(), "no tunnel may be built without the auth token")
}

func TestReconcile_UnknownProviderIsFailedAndNamesIt(t *testing.T) {
	pe := publicEndpointFixture(t)
	pe.Spec.Provider = "nosuch"
	r, c := newReconciler(t, pe, authTokenSecret(t))

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
	require.NoError(t, err)

	var got v1alpha1.PublicEndpoint
	require.NoError(t, c.Get(context.Background(), key(pe), &got))
	assert.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase)
	assert.Contains(t, conditionMessage(&got), "nosuch")

	// The Secret DOES exist in this fixture, so Failed can only be the
	// provider's doing — not a missing credential wearing the wrong phase.
	ready := conditions.Find(got.Status.Conditions, v1alpha1.PublicEndpointConditionReady)
	require.NotNil(t, ready, "the Ready condition must be stamped")
	assert.Equal(t, v1alpha1.ReasonPublicEndpointProviderUnknown, ready.Reason,
		"Failed here must be the unknown provider, not a missing Secret")
}

// TestNoNetworkTunnelProviderIsLinked is the structural guarantee that these
// tests cannot reach the Internet: the only network-talking provider, ngrok,
// is not linked into this binary, so no fixture — however mistyped — can
// resolve to it.
func TestNoNetworkTunnelProviderIsLinked(t *testing.T) {
	assert.NotContains(t, registry.Names(), "ngrok",
		"pkg/web/localtunnel/ngrok must not be linked into this test binary: it is a live network client")
	assert.Contains(t, registry.Names(), testProviderName,
		"the fake provider must be registered, or every test below resolves nothing")
}

func TestReconcile_ReadyPublishesTheProviderURLAndForwardsToTheTargetService(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	res := reconcileOnce(t, r, pe)
	assert.Zero(t, res.RequeueAfter, "a Ready endpoint needs no timed re-check")

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhaseReady, got.Status.Phase)
	assert.Equal(t, publicURL, got.Status.URL, "status.url must be the URL the provider returned")
	require.NotNil(t, got.Status.ObservedAt, "observedAt is stamped when url/phase change")
	assert.True(t, got.Status.ObservedAt.Time.Equal(fixedNow),
		"observedAt must come from the controller's clock, got %v", got.Status.ObservedAt)
	assert.True(t, conditions.IsTrue(got.Status.Conditions, v1alpha1.PublicEndpointConditionReady))

	// Exactly one tunnel, pointed at the Service named in spec.target, opened
	// with the token read out of the Secret.
	require.Equal(t, 1, prov.builtCount(), "one tunnel for one endpoint")
	assert.Equal(t, "http://spicebox-webd.agentprimitives-system.svc.cluster.local:8443",
		prov.tunnelAt(t, 0).LocalAddr(), "the tunnel must forward to spec.target's cluster DNS name and port")
	assert.True(t, prov.tunnelAt(t, 0).Started(), "the tunnel must actually have been started")
	opts := prov.optionsAt(t, 0)
	assert.Equal(t, authTokenValue, opts.AuthToken,
		"the provider must receive the token from spec.authTokenRef's Secret key")
}

// TestReconcile_SecondReconcileReusesTheOpenTunnel is the anti-duplicate-session
// guard: Reconcile runs repeatedly (resyncs, unrelated watch events), and a
// second provider session per endpoint would exhaust a provider's session
// budget and churn the published URL for no reason.
func TestReconcile_SecondReconcileReusesTheOpenTunnel(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c, statusWrites := newReconcilerCountingStatusWrites(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)
	require.Equal(t, 1, prov.builtCount(), "first reconcile opens the tunnel")
	require.Equal(t, int64(1), statusWrites.Load(), "first reconcile publishes the URL")

	// A subsequent build would hand back a DIFFERENT URL, so a reopen cannot
	// hide behind an identical answer.
	prov.setURL(reopenedURL)

	reconcileOnce(t, r, pe)
	assert.Equal(t, 1, prov.builtCount(), "an unchanged spec must not open a second provider session")
	assert.False(t, prov.tunnelAt(t, 0).Stopped(), "the open tunnel must not be torn down by a no-op reconcile")
	assert.Equal(t, int64(1), statusWrites.Load(),
		"an unchanged status must not be re-written: it churns the object and re-triggers every watcher")

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, publicURL, got.Status.URL, "the published URL must still be the original tunnel's")
}

// TestReconcile_SpecChangeReopensTheTunnel is the other half of the guard
// above: a controller that simply never reopens would also pass that test, and
// would strand the endpoint on a stale tunnel after an edit.
func TestReconcile_SpecChangeReopensTheTunnel(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c, statusWrites := newReconcilerCountingStatusWrites(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)
	require.Equal(t, 1, prov.builtCount())
	before := loadEndpoint(t, c, pe)

	// Edit the spec. The reopen guard keys on metadata.generation, which a real
	// apiserver bumps on every spec change of a status-subresource CR; the fake
	// client does not manage generation at all, so the bump is applied here by
	// hand. Reloading and asserting it landed keeps the test from silently
	// exercising the unchanged-spec path instead.
	edited := before.DeepCopy()
	edited.Spec.Target.Port = 9999
	edited.Generation = before.Generation + 1
	require.NoError(t, c.Update(context.Background(), edited))
	afterEdit := loadEndpoint(t, c, pe)
	require.NotEqual(t, before.Generation, afterEdit.Generation,
		"the edited generation must have persisted; the reopen guard keys on it")
	require.Equal(t, int32(9999), afterEdit.Spec.Target.Port, "the spec edit must have persisted")

	prov.setURL(reopenedURL)
	reconcileOnce(t, r, pe)

	assert.Equal(t, 2, prov.builtCount(), "a changed spec must open a fresh tunnel")
	assert.True(t, prov.tunnelAt(t, 0).Stopped(), "the superseded tunnel must be stopped, not leaked")
	assert.Equal(t, "http://spicebox-webd.agentprimitives-system.svc.cluster.local:9999",
		prov.tunnelAt(t, 1).LocalAddr(), "the new tunnel must forward to the edited target")

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, reopenedURL, got.Status.URL, "the new tunnel's URL must be republished")
	assert.Equal(t, int64(2), statusWrites.Load(), "the changed URL is a status write")
}

// TestReconcile_DeleteStopsTheTunnelAndReleasesTheObject covers the leak the
// finalizer exists to prevent: without it the CR vanishes and the provider
// session stays open for the operator's lifetime, referenced by nothing.
func TestReconcile_DeleteStopsTheTunnelAndReleasesTheObject(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)
	require.Equal(t, 1, prov.builtCount())
	require.False(t, prov.tunnelAt(t, 0).Stopped(), "still open before the delete")

	require.NoError(t, c.Delete(context.Background(), pe.DeepCopy()))
	reconcileOnce(t, r, pe)

	assert.True(t, prov.tunnelAt(t, 0).Stopped(), "deleting the CR must stop its tunnel")

	// Removing the last finalizer releases a deletion-timestamped object, so
	// the object is gone. Asserted unconditionally — a "check it only if it is
	// still there" guard would pass whether or not the finalizer was removed.
	var got v1alpha1.PublicEndpoint
	err := c.Get(context.Background(), key(pe), &got)
	require.Error(t, err, "the object must be released, not left held by a finalizer")
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound after the finalizer was removed, got %v", err)
}

func TestReconcile_AddsFinalizerBeforeOpeningTheTunnel(t *testing.T) {
	pe := publicEndpointFixture(t)
	pe.Finalizers = nil
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	reconcileOnce(t, r, pe)

	got := loadEndpoint(t, c, pe)
	assert.True(t, controllerutil.ContainsFinalizer(&got, v1alpha1.FinalizerPublicEndpoint),
		"a tunnel may not be opened before the CR carries the finalizer that guarantees its teardown")
	assert.Zero(t, prov.builtCount(), "the finalizer pass must not also open the tunnel")

	// The requeued pass opens it.
	reconcileOnce(t, r, pe)
	assert.Equal(t, 1, prov.builtCount())
	assert.Equal(t, publicURL, loadEndpoint(t, c, pe).Status.URL)
}

func TestReconcile_ProviderStartFailureIsFailedAndPublishesNoURL(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c, statusWrites := newReconcilerCountingStatusWrites(t, pe, authTokenSecret(t))
	prov := activeFake
	prov.failStart(errFakeStart)

	res := reconcileOnce(t, r, pe)
	assert.Positive(t, res.RequeueAfter, "a provider that refused must be retried")

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase)
	assert.Empty(t, got.Status.URL, "a failed Start must publish no URL")
	assert.Contains(t, conditionMessage(&got), errFakeStart.Error(),
		"the provider's own error must reach the operator")
	require.Equal(t, int64(1), statusWrites.Load())

	// The failed open must not be left claimed: the next reconcile has to be
	// free to try again.
	reconcileOnce(t, r, pe)
	assert.Equal(t, 2, prov.builtCount(), "a failed open must not wedge the endpoint against a retry")
}

// TestReconcile_TheTunnelsContextOutlivesTheReconcileThatOpenedIt guards the
// package comment's fourth rule.
//
// A provider keeps the context handed to Start for the tunnel's whole life
// (ngrok's forward loop returns on ctx.Done). controller-runtime's reconcile
// context is request-scoped and is cancelled on Reconcile's return as soon as
// the manager sets a ReconciliationTimeout — a standard hardening knob this
// operator happens not to set today. Hand the provider that context and every
// tunnel dies the instant it is published, while the CR still reports Ready
// with a URL that forwards nothing and claimOpen short-circuits every later
// reconcile without ever consulting the provider. Nothing would notice.
func TestReconcile_TheTunnelsContextOutlivesTheReconcileThatOpenedIt(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	// Reconcile under a cancellable context, cancelled on return exactly as
	// controller-runtime cancels it when ReconciliationTimeout > 0.
	reconcileCtx, cancelReconcile := context.WithCancel(context.Background())
	_, err := r.Reconcile(reconcileCtx, reconcile.Request{NamespacedName: key(pe)})
	require.NoError(t, err)
	cancelReconcile()

	tunnelCtx := prov.tunnelAt(t, 0).StartCtx()
	require.NotNil(t, tunnelCtx, "the provider must have been handed a context")
	assert.NoError(t, tunnelCtx.Err(),
		"the tunnel's context must outlive the reconcile that opened it: the provider keeps it for the tunnel's whole life, and the reconcile context is request-scoped")

	// The CR meanwhile advertises the tunnel as live — which is precisely what
	// makes a dead context a silent total failure rather than a visible one.
	assert.Equal(t, publicURL, loadEndpoint(t, c, pe).Status.URL)

	// ...and our OWN teardown does end it. Without this half, a controller that
	// simply handed the provider context.Background() would pass above while
	// leaking a context per tunnel.
	require.NoError(t, c.Delete(context.Background(), pe.DeepCopy()))
	reconcileOnce(t, r, pe)
	assert.Error(t, tunnelCtx.Err(), "stopping the tunnel must cancel the context it was opened with")
}

// TestReconcile_AConcurrentReconcileWaitsInsteadOfOpeningASecondTunnel covers
// the `opening` reservation in claim — the mechanism that makes "never hold the
// map lock across Start" and "never open two tunnels" compatible.
//
// controller-runtime already serializes reconciles per object key, so this
// cannot arise in production today and the reservation is defense in depth.
// Untested defense in depth is indistinguishable from dead code to the next
// reader, and deleting the reservation line left the rest of this suite green.
func TestReconcile_AConcurrentReconcileWaitsInsteadOfOpeningASecondTunnel(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake
	entered, release := prov.gateStart()

	// Open the gate no matter how this test exits, so a failure never strands
	// the parked goroutine.
	var releaseOnce sync.Once
	openGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(openGate)

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

	// Reconcile #2 overlaps it. Run in its own goroutine with a deadline: if
	// the reservation is gone this call proceeds into the provider and parks on
	// the same gate, and a bare synchronous call would hang the suite rather
	// than report why.
	type reconcileResult struct {
		res reconcile.Result
		err error
	}
	secondDone := make(chan reconcileResult, 1)
	go func() {
		res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
		secondDone <- reconcileResult{res, err}
	}()

	var second reconcileResult
	select {
	case second = <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the overlapping reconcile blocked inside the provider: it opened a SECOND tunnel instead of deferring to the `opening` reservation")
	}
	require.NoError(t, second.err)

	assert.Positive(t, second.res.RequeueAfter, "the overlapping reconcile must come back later, not proceed")
	assert.Equal(t, 1, prov.builtCount(), "the overlapping reconcile must not open a second provider session")

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhasePending, got.Status.Phase)
	assert.Empty(t, got.Status.URL, "no URL may be published while the tunnel is still opening")
	ready := conditions.Find(got.Status.Conditions, v1alpha1.PublicEndpointConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, v1alpha1.ReasonPublicEndpointTunnelOpening, ready.Reason)

	// Release the first open and let it settle.
	openGate()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the first reconcile never returned after the gate opened")
	}

	// One tunnel, and it is the one that is published.
	reconcileOnce(t, r, pe)
	assert.Equal(t, 1, prov.builtCount(), "still exactly one tunnel after the open settled")
	assert.Equal(t, publicURL, loadEndpoint(t, c, pe).Status.URL)
}

// TestReconcile_ReservedDomainIsRefusedUntilAProviderHonorsIt pins the
// fail-closed refusal. No registered provider wires spec.reservedDomain
// through yet, so accepting it would hand back a per-session URL that changes
// on every restart while the spec promises a pinned hostname. A field accepted
// and ignored is worse than one refused.
func TestReconcile_ReservedDomainIsRefusedUntilAProviderHonorsIt(t *testing.T) {
	pe := publicEndpointFixture(t)
	pe.Spec.ReservedDomain = reservedDomain
	r, c := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake

	res := reconcileOnce(t, r, pe)
	assert.Zero(t, res.RequeueAfter, "a permanent config refusal must not requeue; a spec edit re-triggers the watch")

	got := loadEndpoint(t, c, pe)
	assert.Equal(t, v1alpha1.PublicEndpointPhaseFailed, got.Status.Phase)
	assert.Empty(t, got.Status.URL)
	ready := conditions.Find(got.Status.Conditions, v1alpha1.PublicEndpointConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, v1alpha1.ReasonPublicEndpointReservedDomainUnsupported, ready.Reason)
	assert.Contains(t, ready.Message, reservedDomain, "the operator must be told which value is refused")
	assert.Contains(t, ready.Message, "reservedDomain", "the operator must be told which field is refused")

	// The whole point of refusing: no tunnel is opened, so the endpoint never
	// silently comes up on a URL that is not the one that was asked for.
	assert.Zero(t, prov.builtCount(),
		"a refused reservedDomain must prevent the open entirely — opening a per-session URL anyway is the silent broken promise this refuses")
}

// TestStopAll_AnOpenThatFinishesAfterShutdownIsClosedNotLeaked covers the
// shutdown race: an open still inside the provider's Start when the manager
// stops returns afterwards and tries to record itself.
//
// Two distinct bugs live here. Draining the map to nil made that recording
// panic outright ("assignment to entry in nil map"), and even without the
// panic the tunnel it opened was never stopped — stopAll had already walked
// the map and skipped the entry as a bare reservation, so a live provider
// session survived shutdown with nothing tracking it.
func TestStopAll_AnOpenThatFinishesAfterShutdownIsClosedNotLeaked(t *testing.T) {
	pe := publicEndpointFixture(t)
	r, _ := newReconciler(t, pe, authTokenSecret(t))
	prov := activeFake
	entered, release := prov.gateStart()

	var releaseOnce sync.Once
	openGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(openGate)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		// The reconcile returns Failed ("operator is shutting down"), which is
		// honest; what must NOT happen is a panic or a surviving session.
		_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key(pe)})
		assert.NoError(t, err, "a shutdown race must not surface as a reconcile error")
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile never reached the provider's Start")
	}

	// Shut down while the open is in flight, then let it finish.
	r.stopAll(context.Background())
	openGate()

	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile never returned after shutdown")
	}

	require.Equal(t, 1, prov.builtCount())
	assert.True(t, prov.tunnelAt(t, 0).Stopped(),
		"a tunnel that finished opening after shutdown must be closed by its opener, not left running with nothing tracking it")
}
