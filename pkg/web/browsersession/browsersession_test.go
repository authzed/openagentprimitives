package browsersession

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// testNS is the fabricated namespace every fixture in this file lives in —
// never a real example/deployment namespace (AGENTS.md's no-examples rule).
const testNS = "demo-ns"

// --- fakes -------------------------------------------------------------

// fakeDeps is a minimal Deps: a k8s client and a Granter, both settable to
// nil so the fail-closed table below can exercise Create's own nil guards
// rather than a nil-pointer panic reaching the test.
type fakeDeps struct {
	k8s     client.Client
	granter authz.Granter
	logger  logr.Logger
	checker StartChecker
}

func (d *fakeDeps) K8s() client.Client   { return d.k8s }
func (d *fakeDeps) Authz() authz.Granter { return d.granter }
func (d *fakeDeps) Logger() logr.Logger {
	if d.logger.GetSink() != nil {
		return d.logger
	}
	return logr.Discard()
}

// StartChecker returns the interface field as-is: a nil field is a genuine
// nil interface, which is what lets the fail-closed case above be honest.
func (d *fakeDeps) StartChecker() StartChecker { return d.checker }

// touchCall records one fakeGranter.TouchStartedBy invocation.
type touchCall struct {
	ns, name  string
	canonical identity.CanonicalUserID
}

// fakeGranter is an authz.Granter whose only real method is TouchStartedBy —
// the only one Create calls. err lets a test make the write fail without
// standing up SpiceDB.
type fakeGranter struct {
	mu      sync.Mutex
	touched []touchCall
	err     error
}

func (g *fakeGranter) TouchStartedBy(_ context.Context, ns, name string, canonicalID identity.CanonicalUserID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.touched = append(g.touched, touchCall{ns: ns, name: name, canonical: canonicalID})
	return g.err
}
func (g *fakeGranter) TouchOwner(context.Context, string, string, string) error { return nil }
func (g *fakeGranter) TouchInteractParticipant(context.Context, string, string, string) error {
	return nil
}
func (g *fakeGranter) TouchInteractParticipantUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *fakeGranter) TouchDeniedUser(context.Context, string, string, identity.CanonicalUserID) error {
	return nil
}
func (g *fakeGranter) TouchInteractor(context.Context, string, string, string) error { return nil }

var _ authz.Granter = (*fakeGranter)(nil)

// newFakeK8sClient builds a scheme-aware fake client, pre-seeded with objs.
//
// It also assigns a UID on every Create — the real API server does this and
// this package's whole point is the owner-reference chain (Channel owns
// Secret + AgentSession), so a fake client that leaves every UID "" would
// make every owner-ref assertion below pass trivially on two empty strings.
func newFakeK8sClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID(obj.GetName() + "-generated-uid"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
}

// newFakeInterceptedK8sClient is newFakeK8sClient's raw form: no UID
// auto-assignment, so a test can install its OWN interceptor (a targeted
// Create/Get failure) without it being overridden.
func newFakeInterceptedK8sClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

// demoAgentClass is a fabricated AgentClass fixture with an explicit UID (the
// fake client does not synthesize one for objects seeded via WithObjects), so
// the Channel's owner-ref-to-AgentClass assertion is checked against a real
// value rather than two empty strings.
func demoAgentClass(name string, valid bool) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: types.UID(name + "-fixed-uid")},
	}
	status, reason := metav1.ConditionFalse, "NotReady"
	if valid {
		status, reason = metav1.ConditionTrue, "Valid"
	}
	ac.Status.Conditions = []metav1.Condition{{Type: spiceboxv1alpha1.AgentClassConditionValid, Status: status, Reason: reason}}
	return ac
}

// subjectFor mints a Subject the way webui.SubjectFromContext does: "user:"
// plus the base64url body of an email.
func subjectFor(email string) identity.Subject {
	return identity.Subject("user:" + base64.RawURLEncoding.EncodeToString([]byte(email)))
}

// assertZeroObjects fails the test if any Channel, Secret, or AgentSession
// exists in testNS — the "nothing was written" half of a fail-closed case,
// which matters as much as the returned error: an error returned AFTER a
// partial write is the orphan case this package exists to prevent.
func assertZeroObjects(t *testing.T, k8s client.Client) {
	t.Helper()
	var chans spiceboxv1alpha1.ChannelList
	require.NoError(t, k8s.List(context.Background(), &chans, client.InNamespace(testNS)))
	assert.Empty(t, chans.Items, "no Channel may exist on a fail-closed path")

	var secs corev1.SecretList
	require.NoError(t, k8s.List(context.Background(), &secs, client.InNamespace(testNS)))
	assert.Empty(t, secs.Items, "no Secret may exist on a fail-closed path")

	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, k8s.List(context.Background(), &sessions, client.InNamespace(testNS)))
	assert.Empty(t, sessions.Items, "no AgentSession may exist on a fail-closed path")
}

// --- happy path: per-field characterization -----------------------------

// TestCreate_HappyPath pins the exact object shape Create writes, asserting
// each field independently rather than one assert.Equal on a whole object —
// a whole-object compare fails as a single opaque diff and cannot show which
// of several possible regressions fired.
func TestCreate_HappyPath(t *testing.T) {
	ac := demoAgentClass("demo-agent", true)
	k8s := newFakeK8sClient(t, ac)
	granter := &fakeGranter{}
	deps := &fakeDeps{k8s: k8s, granter: granter}

	// Mixed-case email: Principal.Canonical() lowercases before encoding, so
	// this subject's raw form and its canonicalized form differ. That gap is
	// what makes the "AnnotationStartedByCanonicalID == Created.Owner"
	// assertion below meaningful — with an already-lowercase fixture, a
	// regression that returned the raw Params.Subject as Created.Owner would
	// pass this assertion by coincidence (both values happening to match).
	const rawEmail = "Mixed@Example.COM"
	subject := subjectFor(rawEmail)
	wantCanonical, err := identity.IdPUser(identity.Email(rawEmail), true, "").Canonical()
	require.NoError(t, err)
	wantOwner := wantCanonical.Subject()
	require.NotEqual(t, subject, wantOwner, "fixture must have a non-trivial canonicalization round trip")

	created, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello there",
		Subject:     subject,
		SessionName: "demo-session",
	})
	require.NoError(t, err)

	// --- Channel ---
	ch := created.Channel
	require.NotNil(t, ch)
	require.NotEmpty(t, ch.UID, "the fake client must assign a UID on Create for the owner-ref checks below to be meaningful")
	assert.Equal(t, "demo-session-chan", ch.Name)
	assert.Equal(t, browser.KindName, ch.Spec.Kind)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleBoth, ch.Spec.Role)
	assert.Equal(t, "demo-agent", ch.Spec.AgentClass)
	assert.Equal(t, "user", ch.Spec.SessionScope)
	assert.Equal(t, "demo-session-chan-creds", ch.Spec.CredentialsRef.SecretName)
	require.Len(t, ch.OwnerReferences, 1)
	assert.Equal(t, "AgentClass", ch.OwnerReferences[0].Kind)
	assert.Equal(t, "demo-agent", ch.OwnerReferences[0].Name)
	assert.Equal(t, ac.UID, ch.OwnerReferences[0].UID)

	// --- Secret ---
	sec := created.Creds
	require.NotNil(t, sec)
	assert.Equal(t, "demo-session-chan-creds", sec.Name)
	require.Len(t, sec.OwnerReferences, 1)
	assert.Equal(t, "Channel", sec.OwnerReferences[0].Kind)
	assert.Equal(t, ch.Name, sec.OwnerReferences[0].Name)
	assert.Equal(t, ch.UID, sec.OwnerReferences[0].UID)

	// --- AgentSession labels ---
	sess := created.Session
	require.NotNil(t, sess)
	wantKey := browser.ChannelKey(testNS, ch.Name)
	assert.Equal(t, ch.Name, sess.Labels[spiceboxv1alpha1.LabelChannelName])
	assert.Equal(t, browser.KindName, sess.Labels[spiceboxv1alpha1.LabelChannelKind])
	assert.Equal(t, channelkey.LabelValue(wantKey), sess.Labels[spiceboxv1alpha1.LabelChannelKey],
		"LabelChannelKey must be the hash of spec.inputChannel.Key -- the correlation key the inbound pipeline matches on")

	// --- AgentSession owner ref (Channel owns AgentSession, so deleting the
	// Channel cascades the AgentSession's deletion via K8s GC) ---
	require.Len(t, sess.OwnerReferences, 1)
	assert.Equal(t, "Channel", sess.OwnerReferences[0].Kind)
	assert.Equal(t, ch.Name, sess.OwnerReferences[0].Name)
	assert.Equal(t, ch.UID, sess.OwnerReferences[0].UID)

	// --- AgentSession annotations ---
	assert.Equal(t, rawEmail, sess.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID])
	assert.Equal(t, rawEmail, sess.Annotations[spiceboxv1alpha1.AnnotationStartedByEmail])
	assert.Equal(t, wantOwner.String(), sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
	assert.Equal(t, sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID], created.Owner.String(),
		"Created.Owner must equal the annotation Create actually stamped — dormantForOwner's owner check depends on this equality")

	// --- AgentSession spec ---
	assert.Equal(t, "demo-agent", sess.Spec.Class)
	assert.Equal(t, "hello there", sess.Spec.Prompt.Inline)
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, ch.Name, sess.Spec.InputChannel.Name)
	assert.Equal(t, browser.KindName, sess.Spec.InputChannel.Kind)
	assert.Equal(t, wantKey, sess.Spec.InputChannel.Key)
	assert.Equal(t, (&browser.Kind{}).Capabilities(), sess.Spec.InputChannel.Capabilities)
	assert.Equal(t, channelevents.SubjectPrefix(testNS, "demo-session"), sess.Spec.InputChannel.NATSSubjectPrefix)

	// --- authz ---
	require.Len(t, granter.touched, 1, "TouchStartedBy must be called exactly once")
	assert.Equal(t, testNS, granter.touched[0].ns)
	assert.Equal(t, "demo-session", granter.touched[0].name)
	assert.Equal(t, wantCanonical, granter.touched[0].canonical)
}

// --- fail-closed table ----------------------------------------------------

func TestCreate_FailClosed(t *testing.T) {
	cases := []struct {
		name            string
		seed            []client.Object
		nilK8s          bool
		granter         authz.Granter
		prompt          string
		wantErrContains string
		wantErrIs       error
	}{
		{
			name:            "nil Granter: refuses before writing anything",
			seed:            []client.Object{demoAgentClass("demo-agent", true)},
			granter:         nil,
			prompt:          "hello",
			wantErrContains: "Granter",
		},
		{
			name:            "nil K8s: refuses",
			nilK8s:          true,
			granter:         &fakeGranter{},
			prompt:          "hello",
			wantErrContains: "K8s",
		},
		{
			name:            "blank Prompt: refuses before writing anything",
			seed:            []client.Object{demoAgentClass("demo-agent", true)},
			granter:         &fakeGranter{},
			prompt:          "   ",
			wantErrContains: "Prompt",
		},
		{
			name:      "AgentClass absent, or present with Valid=False",
			seed:      nil, // absent
			granter:   &fakeGranter{},
			prompt:    "hello",
			wantErrIs: ErrUnknownAgentClass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var k8s client.Client
			if !tc.nilK8s {
				k8s = newFakeK8sClient(t, tc.seed...)
			}
			deps := &fakeDeps{k8s: k8s, granter: tc.granter}

			_, err := Create(context.Background(), deps, Params{
				Namespace:   testNS,
				AgentClass:  "demo-agent",
				Prompt:      tc.prompt,
				Subject:     subjectFor("demo@example.com"),
				SessionName: "demo-session",
			})
			require.Error(t, err)
			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
			}
			if tc.wantErrContains != "" {
				assert.ErrorContains(t, err, tc.wantErrContains)
			}
			if !tc.nilK8s {
				assertZeroObjects(t, k8s)
			}
		})
	}
}

// TestCreate_AgentClassNotReady_ZeroObjectsCreated covers the "present with
// Valid=False" half of the fail-closed table's combined last row (kept out of
// the table itself so the table stays at the four cases named in the task
// brief; this uses the same helpers and assertion shape).
func TestCreate_AgentClassNotReady_ZeroObjectsCreated(t *testing.T) {
	k8s := newFakeK8sClient(t, demoAgentClass("demo-agent", false))
	deps := &fakeDeps{k8s: k8s, granter: &fakeGranter{}}

	_, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello",
		Subject:     subjectFor("demo@example.com"),
		SessionName: "demo-session",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownAgentClass)
	assertZeroObjects(t, k8s)
}

// TestCreate_AgentClassReadFails_IsNotAVerdictOnTheClass is the split that
// keeps a caller from telling a viewer their agent is broken when the control
// plane merely failed to answer.
//
// ErrUnknownAgentClass means "this class does not exist, or exists and is not
// Valid" — a verdict. A Get that FAILS (throttling, a timeout, an RBAC
// refusal, a webhook stall) is not a verdict, and folding it into the sentinel
// is what makes both start routes answer 409 "this agent is not ready" and log
// that the class is not ready, neither of which is true.
//
// The assertion is two-sided on purpose. Asserting only "an error is returned"
// would hold before the fix as well, because the pre-fix code also errored —
// with the wrong error. The discriminating half is NotErrorIs.
func TestCreate_AgentClassReadFails_IsNotAVerdictOnTheClass(t *testing.T) {
	readErr := errors.New("etcdserver: request timed out")
	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*spiceboxv1alpha1.AgentClass); ok {
				return readErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, demoAgentClass("demo-agent", true)) // the class EXISTS and is Valid
	deps := &fakeDeps{k8s: k8s, granter: &fakeGranter{}}

	_, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello",
		Subject:     subjectFor("demo@example.com"),
		SessionName: "demo-session",
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnknownAgentClass,
		"a failed read is not a verdict on the class — callers map this sentinel to \"this agent is not ready\", which would be false")
	assert.ErrorIs(t, err, readErr, "the real cause must survive for the caller's log")
	assert.ErrorContains(t, err, "demo-agent", "the wrapped message must name what was being read")
	assertZeroObjects(t, k8s)
}

// TestCreate_AgentClassAbsent_IsAVerdict is the other half: a genuine NotFound
// IS the sentinel, so the 409 arm both start routes keep for it (and the
// shared refusal copy that makes an unknown class indistinguishable from an
// unauthorized one) still fires.
func TestCreate_AgentClassAbsent_IsAVerdict(t *testing.T) {
	k8s := newFakeK8sClient(t) // nothing seeded: the Get is a genuine NotFound
	deps := &fakeDeps{k8s: k8s, granter: &fakeGranter{}}

	_, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello",
		Subject:     subjectFor("demo@example.com"),
		SessionName: "demo-session",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownAgentClass)
	assertZeroObjects(t, k8s)
}

// --- rollback -------------------------------------------------------------

// TestCreate_AgentSessionCreateFails_RollsBackChannelAndSecret: an error
// BEFORE the AgentSession exists must leave no orphan — the Channel and its
// creds Secret are best-effort deleted.
func TestCreate_AgentSessionCreateFails_RollsBackChannelAndSecret(t *testing.T) {
	ac := demoAgentClass("demo-agent", true)
	createErr := errors.New("injected AgentSession create failure")
	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isSession := obj.(*spiceboxv1alpha1.AgentSession); isSession {
				return createErr
			}
			return c.Create(ctx, obj, opts...)
		},
	}, ac)
	granter := &fakeGranter{}
	deps := &fakeDeps{k8s: k8s, granter: granter}

	_, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello",
		Subject:     subjectFor("demo@example.com"),
		SessionName: "demo-session",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "create AgentSession")

	chErr := k8s.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "demo-session-chan"}, &spiceboxv1alpha1.Channel{})
	assert.True(t, apierrors.IsNotFound(chErr), "the Channel must be rolled back when the AgentSession create fails, got err=%v", chErr)

	secErr := k8s.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "demo-session-chan-creds"}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(secErr), "the creds Secret must be rolled back when the AgentSession create fails, got err=%v", secErr)

	assert.Empty(t, granter.touched, "TouchStartedBy must never be called when the AgentSession was never created")
}

// TestCreate_TouchStartedByFails_NeverDeletesTheCreatedSession is the mirror
// case: once the AgentSession exists, NOTHING is rolled back on a later
// error — its lifecycle belongs to the operator from here. This is the
// invariant a well-meaning refactor is most likely to break.
func TestCreate_TouchStartedByFails_NeverDeletesTheCreatedSession(t *testing.T) {
	ac := demoAgentClass("demo-agent", true)
	k8s := newFakeK8sClient(t, ac)
	granter := &fakeGranter{err: errors.New("injected spicedb write failure")}
	deps := &fakeDeps{k8s: k8s, granter: granter}

	_, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello",
		Subject:     subjectFor("demo@example.com"),
		SessionName: "demo-session",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "started_by")

	assert.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "demo-session-chan"}, &spiceboxv1alpha1.Channel{}),
		"the Channel must NOT be deleted once the AgentSession exists")
	assert.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "demo-session-chan-creds"}, &corev1.Secret{}),
		"the creds Secret must NOT be deleted once the AgentSession exists")
	assert.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "demo-session"}, &spiceboxv1alpha1.AgentSession{}),
		"the AgentSession itself must NOT be deleted — its lifecycle belongs to the operator from here")
}

// TestCreate_RollbackDeleteFailure_IsLogged: a rollback delete that itself
// fails must be SURFACED (logged), never silently dropped (AGENTS.md §Never
// silently drop errors). Without this, an orphaned Channel is invisible — the
// caller sees only the original create error and nothing anywhere records that
// the cleanup for it also failed.
func TestCreate_RollbackDeleteFailure_IsLogged(t *testing.T) {
	// Force the abort right after the Channel is created: the re-Get for the
	// Channel UID fails, so Create returns before writing the creds Secret and
	// the deferred Channel rollback runs. That Delete is made to fail too —
	// with a non-NotFound error, which the logged line must report (a NotFound
	// delete is legitimately suppressed).
	//
	// The Get interceptor is scoped to *Channel* objects only: Create Gets the
	// AgentClass BEFORE the Channel's re-Get, so an unscoped Get failure would
	// abort there instead and never reach the rollback path this test targets.
	var mu sync.Mutex
	var logs []string
	rec := funcr.New(func(_, args string) {
		mu.Lock()
		logs = append(logs, args)
		mu.Unlock()
	}, funcr.Options{})

	getErr := errors.New("injected re-get failure")
	delErr := errors.New("injected delete failure")
	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isChannel := obj.(*spiceboxv1alpha1.Channel); isChannel {
				return getErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return delErr
		},
	}, demoAgentClass("demo-agent", true))

	deps := &fakeDeps{k8s: k8s, granter: &fakeGranter{}, logger: rec}
	_, err := Create(context.Background(), deps, Params{
		Namespace:   testNS,
		AgentClass:  "demo-agent",
		Prompt:      "hello",
		Subject:     subjectFor("demo@example.com"),
		SessionName: "demo-session",
	})
	require.Error(t, err, "Create must fail when the Channel re-Get fails")
	assert.ErrorContains(t, err, "re-get browser Channel")

	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	assert.Contains(t, joined, "rollback delete Channel failed",
		"a failed rollback delete must be surfaced (logged), not silently dropped")
	assert.Contains(t, joined, delErr.Error(),
		"the log must carry the delete error so an operator can locate the failure")
}

// --- DeriveIdentity / NewSessionName / AgentClassReady ---------------------

func TestDeriveIdentity_VerifiedIdPPrincipal(t *testing.T) {
	ext, principal, err := DeriveIdentity(subjectFor("demo@example.com"))
	require.NoError(t, err)
	assert.Equal(t, "demo@example.com", ext.Email.String())
	assert.Equal(t, "demo@example.com", ext.ExternalID.String())
	assert.Equal(t, identity.Email("demo@example.com"), principal.Email())
	assert.True(t, principal.EmailVerified())
	assert.Equal(t, identity.KindIdP, principal.Kind())
}

func TestDeriveIdentity_EmptySubject_ReturnsError(t *testing.T) {
	// An empty subject decodes to an empty email; IdPUser then builds a
	// Principal with no email and no AllowSynthetic opt-in, so
	// Principal.Canonical() (which DeriveIdentity validates internally)
	// returns ErrSyntheticSubject. This is the "Principal.Canonical() can
	// fail" path the package doc calls out.
	_, _, err := DeriveIdentity(identity.Subject(""))
	require.Error(t, err)
	assert.ErrorIs(t, err, identity.ErrSyntheticSubject)
}

func TestNewSessionName_HasAgentClassPrefix(t *testing.T) {
	name := NewSessionName("demo-agent")
	assert.Regexp(t, `^demo-agent-[0-9a-f]{8}$`, name)
}

func TestAgentClassReady(t *testing.T) {
	assert.True(t, AgentClassReady(demoAgentClass("x", true)))
	assert.False(t, AgentClassReady(demoAgentClass("x", false)))
	assert.False(t, AgentClassReady(&spiceboxv1alpha1.AgentClass{}), "no Valid condition at all must not be treated as valid")
}

// --- the start gate: refuse before creating anything -----------------------

// fakeStartChecker is a StartChecker whose answer and error are scripted, and
// which counts its own calls so a test can assert the gate is asked exactly
// once — not zero (skipped) and not per-write (asked again for every object).
type fakeStartChecker struct {
	allow bool
	err   error
	calls int
}

func (f *fakeStartChecker) CheckAgentClassStart(_ context.Context, _, _, _ string, _ identity.CanonicalUserID) (bool, error) {
	f.calls++
	return f.allow, f.err
}

// gatedDemoAgentClass is demoAgentClass("demo-agent", true) — the file's
// healthy fixture — with an explicit allowlist the test subject is not on.
func gatedDemoAgentClass() *spiceboxv1alpha1.AgentClass {
	ac := demoAgentClass("demo-agent", true)
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Session: &spiceboxv1alpha1.SessionAuthz{
		AllowedStarters: []string{"user:listed"},
	}}
	return ac
}

func TestCreate_RefusesAnUnlistedStarterBeforeWritingAnything(t *testing.T) {
	k8s := newFakeK8sClient(t, gatedDemoAgentClass())
	chk := &fakeStartChecker{allow: false}
	deps := &fakeDeps{k8s: k8s, granter: &fakeGranter{}, checker: chk}

	_, err := Create(context.Background(), deps, Params{
		Namespace: testNS, AgentClass: "demo-agent", Prompt: "hi", Subject: identity.Subject("user:stranger@example.com"),
	})
	require.ErrorIs(t, err, ErrNotAnAllowedStarter)
	assert.Equal(t, 1, chk.calls, "the gate was asked exactly once")
	assertZeroObjects(t, k8s)
}

func TestCreate_NilStartCheckerFailsClosedOnlyForGatedClasses(t *testing.T) {
	// Ungated class, no checker wired: proceeds exactly as before this change.
	openK8s := newFakeK8sClient(t, demoAgentClass("demo-agent", true))
	_, err := Create(context.Background(), &fakeDeps{k8s: openK8s, granter: &fakeGranter{}}, Params{
		Namespace: testNS, AgentClass: "demo-agent", Prompt: "hi", Subject: identity.Subject("user:anyone@example.com"),
	})
	require.NoError(t, err)

	// Gated class, no checker wired: refused, and nothing written.
	gatedK8s := newFakeK8sClient(t, gatedDemoAgentClass())
	_, err = Create(context.Background(), &fakeDeps{k8s: gatedK8s, granter: &fakeGranter{}}, Params{
		Namespace: testNS, AgentClass: "demo-agent", Prompt: "hi", Subject: identity.Subject("user:anyone@example.com"),
	})
	require.ErrorIs(t, err, ErrNotAnAllowedStarter)
	assertZeroObjects(t, gatedK8s)
}

// TestCreate_GatedClassCheckerErrors_ErrorReturnedUnmappedNothingWritten
// covers the branch TestCreate_RefusesAnUnlistedStarterBeforeWritingAnything
// and TestCreate_NilStartCheckerFailsClosedOnlyForGatedClasses do not reach:
// the checker itself FAILING (a SpiceDB outage, a timeout) rather than
// answering false.
//
// That failure must NOT be folded into ErrNotAnAllowedStarter — a caller
// mapping that sentinel to a refusal (pkg/web/webui/sessions/start.go does,
// on purpose, with the same copy as an unknown class) would tell a viewer
// "you are not allowed" when the true answer is "nobody could find out". The
// checker's own cause and the "start gate" framing must both survive in the
// returned error for the operator's log, kept out of the table above (whose
// name/wantErrContains/wantErrIs shape has no way to assert a NEGATIVE
// ErrorIs alongside a scripted checker) rather than bolted onto it as fields
// every other row would leave unused.
func TestCreate_GatedClassCheckerErrors_ErrorReturnedUnmappedNothingWritten(t *testing.T) {
	k8s := newFakeK8sClient(t, gatedDemoAgentClass())
	checkerErr := errors.New("spicedb unreachable")
	chk := &fakeStartChecker{err: checkerErr}
	deps := &fakeDeps{k8s: k8s, granter: &fakeGranter{}, checker: chk}

	_, err := Create(context.Background(), deps, Params{
		Namespace: testNS, AgentClass: "demo-agent", Prompt: "hi", Subject: identity.Subject("user:stranger@example.com"),
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotAnAllowedStarter,
		"a checker FAILURE is not a verdict on the viewer's standing and must not read as a refusal")
	assert.ErrorContains(t, err, "spicedb unreachable", "the checker's own cause must survive for the operator's log")
	assert.ErrorContains(t, err, "start gate", "the error must be identifiable as coming from the start gate")
	assert.Equal(t, 1, chk.calls, "the gate was asked exactly once")
	assertZeroObjects(t, k8s)
}
