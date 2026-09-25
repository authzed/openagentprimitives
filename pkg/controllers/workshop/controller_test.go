package workshop_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeTuples records every call rather than talking to SpiceDB, so unit
// tests can assert layer-1.3 write behavior (once per reconcile, TOUCH
// semantics) without a live backend. The starter is recorded because it is
// what makes the workshop closable by the person who opened it — a reconcile
// that dropped it would leave every other assertion here unchanged.
type fakeTuples struct {
	ensured []string // "<workshopID>|<sessNS>/<sessName>|<starterCanonical>"
	deleted []string
	err     error

	// closeChecks records every CheckWorkshopClose as
	// "<workshopID>|<canonicalID>"; closeAllow answers it per workshop id, so
	// a test can allow one target and deny another. closeErr makes the check
	// itself fail, which is NOT a refusal.
	closeChecks []string
	closeAllow  map[string]bool
	closeErr    error
}

func (f *fakeTuples) EnsureWorkshopSubjects(_ context.Context, ws, ns, name string, starter identity.CanonicalUserID) error {
	f.ensured = append(f.ensured, ws+"|"+ns+"/"+name+"|"+starter.String())
	return f.err
}

func (f *fakeTuples) CheckWorkshopClose(_ context.Context, ws string, who identity.CanonicalUserID) (bool, error) {
	f.closeChecks = append(f.closeChecks, ws+"|"+who.String())
	if f.closeErr != nil {
		return false, f.closeErr
	}
	return f.closeAllow[ws], nil
}

func (f *fakeTuples) DeleteWorkshopRelationships(_ context.Context, ws string) error {
	f.deleted = append(f.deleted, ws)
	return f.err
}

func builderSession(name, uid string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(uid),
		},
	}
}

func sanctionedWorkshop(sess *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.Workshop {
	return &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{
			Name:            spiceboxv1alpha1.WorkshopName(sess.Name),
			Namespace:       "default",
			OwnerReferences: cosidecar.OwnerRef(sess),
		},
		Spec: spiceboxv1alpha1.WorkshopSpec{
			Session:          spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: sess.Name},
			StarterCanonical: "c4nonical",
			SidecarToolbox:   "workshop",
			Limits: spiceboxv1alpha1.WorkshopLimits{
				MaxAge:              metav1.Duration{Duration: 168 * time.Hour},
				MaxObjectsPerKind:   20,
				MaxObjects:          100,
				MaxConcurrentProbes: 2,
			},
		},
	}
}

// newFakeReconciler builds a fake-client-backed Reconciler. tuples/registry
// are passed in explicitly (rather than always constructed) so the
// nil-dependency tests can wire a genuine nil without a special case.
func newFakeReconciler(t *testing.T, tuples workshop.WorkshopTuples, reg *tokens.Registry, objs ...client.Object) (client.Client, *workshop.Reconciler) {
	t.Helper()
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme, networkingv1.AddToScheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.Workshop{}).
		// The fake client's DEFAULT SSA type-converter chain tries
		// client-go's generated apply-configuration schema first, which has a
		// gap for networking.k8s.io/v1.NetworkPolicy in this pinned
		// k8s.io/client-go version: the live (empty) side and the applied side
		// end up parsed against two different internal schema instances and
		// structured-merge-diff refuses to compare them ("expected objects
		// with types from the same schema"). Forcing the reflection-based
		// DeducedTypeConverter for every type sidesteps the gap — both sides
		// are always parsed by the same converter. Apply/SSA against a REAL
		// apiserver (every envtest and production path) is unaffected; this
		// is a fake-client-only test fixture concern.
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		Build()
	r := &workshop.Reconciler{
		Client: c,
		Tuples: tuples,
		Tokens: reg,
	}
	return c, r
}

func reconcileWorkshop(t *testing.T, r *workshop.Reconciler, ws *spiceboxv1alpha1.Workshop) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}})
}

func getWorkshop(t *testing.T, c client.Client, ws *spiceboxv1alpha1.Workshop) *spiceboxv1alpha1.Workshop {
	t.Helper()
	var got spiceboxv1alpha1.Workshop
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}, &got))
	return &got
}

// primeFinalizer drives the one Reconcile call every fresh Workshop needs
// before any provisioning logic runs: Reconcile's step 0 adds
// FinalizerWorkshop via a plain Update and returns {Requeue: true}, nil
// immediately, before even reading spec.session. Every test below primes
// once so its real assertions land on the call that actually reaches
// provisioning.
func primeFinalizer(t *testing.T, r *workshop.Reconciler, ws *spiceboxv1alpha1.Workshop) {
	t.Helper()
	res, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "priming the finalizer must not error")
	assert.True(t, res.Requeue, "priming call should only add the finalizer and requeue")
}

func TestReconcile_ProvisionsEverythingOnce(t *testing.T) {
	sess := builderSession("builder-1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	ws := sanctionedWorkshop(sess)
	ft := &fakeTuples{}
	reg := tokens.NewRegistry()
	c, r := newFakeReconciler(t, ft, reg, sess, ws)
	ctx := context.Background()

	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	saName := spiceboxv1alpha1.WorkshopServiceAccountName(sess.Name)
	secretName := spiceboxv1alpha1.WorkshopTokenSecretName(sess.Name)

	primeFinalizer(t, r, ws)
	res, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "first provisioning reconcile must succeed")
	assert.False(t, res.Requeue)

	// Namespace: both session labels + restricted Pod Security.
	var ns corev1.Namespace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: nsName}, &ns), "workshop namespace must exist")
	assert.Equal(t, sess.Namespace, ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionNamespace])
	assert.Equal(t, sess.Name, ns.Labels[spiceboxv1alpha1.LabelWorkshopSessionName])
	assert.Equal(t, "restricted", ns.Labels["pod-security.kubernetes.io/enforce"])

	// Deny-all NetworkPolicy: empty selector, both types, zero rules.
	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-default-deny"}, &np))
	assert.Empty(t, np.Spec.PodSelector.MatchLabels)
	assert.Empty(t, np.Spec.PodSelector.MatchExpressions)
	assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	assert.Empty(t, np.Spec.Ingress)
	assert.Empty(t, np.Spec.Egress)

	// ResourceQuota exists.
	var rq corev1.ResourceQuota
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-quota"}, &rq))

	// LimitRange exists beside it. The quota demands requests AND limits on
	// every container, so without this the namespace refuses every pod whose
	// author did not set all four by hand.
	var lr corev1.LimitRange
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-limits"}, &lr))
	require.Len(t, lr.Spec.Limits, 1)
	assert.Equal(t, corev1.LimitTypeContainer, lr.Spec.Limits[0].Type)

	// ServiceAccount, in the SESSION's namespace.
	var sa corev1.ServiceAccount
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: saName}, &sa))

	// Role in the workshop namespace grants EXACTLY the eight CRUD kinds plus
	// the agentsessions rule (get/list/watch/delete, for test_sessions and
	// stop_test) plus the narrower workshopprobes rule across its three
	// rules, and nothing in the core group.
	var role rbacv1.Role
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-agent"}, &role))
	wantRules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"agentprimitives.authzed.com"},
			Resources: []string{"agentclasses", "mcpservers", "sidecartoolboxes", "agentidentities", "skills", "agentuis", "subagentrequests"},
			Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
		},
		{
			APIGroups: []string{"agentprimitives.authzed.com"},
			Resources: []string{"agentsessions"},
			Verbs:     []string{"get", "list", "watch", "delete"},
		},
		{
			APIGroups: []string{"agentprimitives.authzed.com"},
			Resources: []string{"workshopprobes"},
			Verbs:     []string{"create", "get", "list", "watch"},
		},
	}
	assert.ElementsMatch(t, wantRules, role.Rules)
	for _, ru := range role.Rules {
		for _, g := range ru.APIGroups {
			assert.NotEqual(t, "", g, "no core-group rule should be granted")
		}
		assert.NotContains(t, ru.Resources, "secrets")
		assert.NotContains(t, ru.Resources, "pods")
		assert.NotContains(t, ru.Resources, "configmaps")
	}

	// Both RoleBindings + the toolwriter/reader ClusterRoleBindings exist.
	var wsBinding rbacv1.RoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "workshop-agent"}, &wsBinding))
	var crBinding rbacv1.RoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: saName + "-cr"}, &crBinding))
	var toolCRB rbacv1.ClusterRoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: workshop.WorkshopToolwriterCRBName("default", sess.Name)}, &toolCRB))
	var readerCRB rbacv1.ClusterRoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: workshop.WorkshopReaderCRBName("default", sess.Name)}, &readerCRB))

	// Tuples written exactly once, for this workshop/session/starter.
	require.Equal(t, []string{nsName + "|default/" + sess.Name + "|" + ws.Spec.StarterCanonical}, ft.ensured)

	// Bearer Secret, non-empty token.
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: secretName}, &sec))
	firstToken := sec.Data["token"]
	assert.NotEmpty(t, firstToken)

	// Registered under the workshop's OWN key, not the session's.
	assert.True(t, reg.Registered(memory.NamespacedName{Namespace: "default", Name: spiceboxv1alpha1.WorkshopName(sess.Name)}))

	// Status.
	got := getWorkshop(t, c, ws)
	assert.Equal(t, nsName, got.Status.Namespace)
	assert.Equal(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase)
	require.NotNil(t, got.Status.ProvisionedAt)
	require.NotNil(t, got.Status.SidecarIdentity)
	assert.Equal(t, saName, got.Status.SidecarIdentity.ServiceAccount)
	assert.Equal(t, secretName, got.Status.SidecarIdentity.TokenSecret)
	for _, condType := range []string{
		spiceboxv1alpha1.WorkshopConditionNamespaceReady,
		spiceboxv1alpha1.WorkshopConditionRBACReady,
		spiceboxv1alpha1.WorkshopConditionTupleWritten,
		spiceboxv1alpha1.WorkshopConditionTokensReady,
	} {
		assert.True(t, testfixtures.HasConditionTrue(got.Status.Conditions, condType), "condition %s should be True", condType)
	}

	// Second reconcile: TOUCH is idempotent (fine to re-ensure), but the
	// bearer must be set-once.
	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err, "second reconcile must succeed")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: secretName}, &sec))
	assert.Equal(t, firstToken, sec.Data["token"], "the bearer token must never be rewritten once minted")
	assert.Len(t, ft.ensured, 2, "re-ensuring the tuple on every reconcile is fine; re-minting the token is not")
}

// The starter tuple is what lets the person who opened a workshop close it
// from another builder, and nothing else in provisioning depends on it: drop
// it and every other assertion in this file still passes, the reconcile still
// reaches Ready, and the only symptom is a person refused their own workshop.
// Pinned here on its own, at both values spec.starterCanonical can hold.
func TestReconcile_PassesTheWorkshopsStarterToTheTupleWrite(t *testing.T) {
	t.Run("a recorded starter is passed through verbatim", func(t *testing.T) {
		sess := builderSession("builder-1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
		ws := sanctionedWorkshop(sess)
		ft := &fakeTuples{}
		_, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws)

		primeFinalizer(t, r, ws)
		_, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err)

		nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
		assert.Equal(t, []string{nsName + "|default/" + sess.Name + "|c4nonical"}, ft.ensured)
	})

	t.Run("no starter recorded: an empty id, never a fabricated one", func(t *testing.T) {
		sess := builderSession("builder-2", "b2c3d4e5-f6a1-7890-abcd-ef1234567890")
		ws := sanctionedWorkshop(sess)
		// spec.starterCanonical is +optional: a session with no started-by
		// annotation leaves it empty. Provisioning must still succeed, and the
		// controller must pass the emptiness on rather than substituting the
		// session or anyone else — attributing the workshop to the wrong
		// person would hand them the close permission.
		ws.Spec.StarterCanonical = ""
		ft := &fakeTuples{}
		_, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws)

		primeFinalizer(t, r, ws)
		_, err := reconcileWorkshop(t, r, ws)
		require.NoError(t, err, "an unattributed workshop must still provision")

		nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
		assert.Equal(t, []string{nsName + "|default/" + sess.Name + "|"}, ft.ensured)
	})
}

// The browser (webd) creates a person's own test session in the workshop
// namespace, which needs the same Channel/Secret/AgentSession writes its
// static start namespaces get — granted here per workshop, removed with the
// namespace. Nothing is granted when no browser service account is wired.
func TestReconcile_BindsTheBrowsersStartRoleIntoTheWorkshop(t *testing.T) {
	sess := builderSession("builder-1", "a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	ws := sanctionedWorkshop(sess)
	c, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), sess, ws)
	r.BrowserServiceAccount, r.BrowserServiceAccountNamespace = "spicebox-webd", "agentprimitives-system"
	ctx := context.Background()
	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)

	var role rbacv1.Role
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workshop.BrowserRoleName}, &role))
	verbs := map[string][]string{}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			verbs[res] = rule.Verbs
		}
	}
	assert.ElementsMatch(t, []string{"get", "list", "create", "delete"}, verbs["channels"])
	assert.ElementsMatch(t, []string{"get", "list", "create", "delete", "patch", "update"}, verbs["agentsessions"])
	assert.ElementsMatch(t, []string{"get", "create", "delete"}, verbs["secrets"])

	var binding rbacv1.RoleBinding
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: nsName, Name: workshop.BrowserRoleName}, &binding))
	assert.Equal(t, "Role", binding.RoleRef.Kind)
	assert.Equal(t, workshop.BrowserRoleName, binding.RoleRef.Name)
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, rbacv1.Subject{Kind: "ServiceAccount", Name: "spicebox-webd", Namespace: "agentprimitives-system"}, binding.Subjects[0])
}

func TestReconcile_NoBrowserServiceAccount_GrantsNothingAndSaysSo(t *testing.T) {
	sess := builderSession("builder-2", "b1b2c3d4-e5f6-7890-abcd-ef1234567890")
	ws := sanctionedWorkshop(sess)
	c, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), sess, ws)
	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err)
	var role rbacv1.Role
	err = c.Get(context.Background(), types.NamespacedName{Namespace: spiceboxv1alpha1.WorkshopNamespaceName(sess.UID), Name: workshop.BrowserRoleName}, &role)
	assert.True(t, apierrors.IsNotFound(err), "no browser SA wired: no Role")
}

func TestReconcile_NilTuplesFailsClosed(t *testing.T) {
	sess := builderSession("builder-2", "b1b2c3d4-e5f6-7890-abcd-ef1234567891")
	ws := sanctionedWorkshop(sess)
	c, r := newFakeReconciler(t, nil, tokens.NewRegistry(), sess, ws)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.Error(t, err, "reconcile must fail closed with a nil Tuples dependency")

	got := getWorkshop(t, c, ws)
	assert.True(t, testfixtures.HasCondition(got.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTupleWritten, metav1.ConditionFalse, spiceboxv1alpha1.ReasonWorkshopProvisionFailed))
	assert.NotEqual(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase)

	secretName := spiceboxv1alpha1.WorkshopTokenSecretName(sess.Name)
	var sec corev1.Secret
	err = c.Get(ctx, types.NamespacedName{Namespace: "default", Name: secretName}, &sec)
	assert.True(t, apierrors.IsNotFound(err), "no bearer Secret should be created when the tuple layer failed first: %v", err)
}

func TestReconcile_NilTokensFailsClosed(t *testing.T) {
	sess := builderSession("builder-3", "c1b2c3d4-e5f6-7890-abcd-ef1234567892")
	ws := sanctionedWorkshop(sess)
	c, r := newFakeReconciler(t, &fakeTuples{}, nil, sess, ws)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.Error(t, err, "reconcile must fail closed with a nil Tokens dependency")

	got := getWorkshop(t, c, ws)
	assert.True(t, testfixtures.HasCondition(got.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTokensReady, metav1.ConditionFalse, spiceboxv1alpha1.ReasonWorkshopProvisionFailed))
	assert.NotEqual(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase)
	assert.Nil(t, got.Status.SidecarIdentity)
}

func TestReconcile_NamespaceCollisionIsAConflictNotAnAdoption(t *testing.T) {
	sess := builderSession("builder-4", "d1b2c3d4-e5f6-7890-abcd-ef1234567893")
	ws := sanctionedWorkshop(sess)
	nsName := spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	collide := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nsName,
			Labels: map[string]string{"owned-by": "someone-else"},
		},
	}
	c, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), sess, ws, collide)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.Error(t, err, "reconcile must refuse an unlabeled namespace collision")
	assert.Contains(t, err.Error(), nsName, "the error should name the colliding namespace")

	got := getWorkshop(t, c, ws)
	assert.True(t, testfixtures.HasCondition(got.Status.Conditions, spiceboxv1alpha1.WorkshopConditionNamespaceReady, metav1.ConditionFalse, spiceboxv1alpha1.ReasonWorkshopProvisionFailed))

	// The load-bearing anchor fact: a collision must be refused BEFORE
	// status.Namespace is ever written, so the anchor never persists a foreign
	// namespace name. teardown deletes status.Namespace by name, so an anchor
	// left pointing at this colliding namespace would let teardown destroy it.
	assert.Empty(t, got.Status.Namespace, "a collision must never anchor status.Namespace at a namespace this workshop does not own")

	var stillThere corev1.Namespace
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: nsName}, &stillThere))
	assert.Equal(t, map[string]string{"owned-by": "someone-else"}, stillThere.Labels, "the pre-existing namespace's labels must be untouched — never adopted, never relabeled")
}

func TestReconcile_SessionGoneIsANoop(t *testing.T) {
	sess := builderSession("builder-5", "e1b2c3d4-e5f6-7890-abcd-ef1234567894")
	ws := sanctionedWorkshop(sess)
	// sess is deliberately NOT created — spec.session names a ghost.
	c, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), ws)
	ctx := context.Background()

	primeFinalizer(t, r, ws)
	res, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "a Workshop whose session is gone must be a clean no-op, not an error")
	assert.Zero(t, res.RequeueAfter)

	var nsList corev1.NamespaceList
	require.NoError(t, c.List(ctx, &nsList))
	assert.Empty(t, nsList.Items, "no namespace should be created for a ghost session")

	got := getWorkshop(t, c, ws)
	assert.Empty(t, got.Status.Phase, "no provisioning should have been attempted")
	assert.Empty(t, got.Status.Conditions)
}

// secretBlindClient delegates everything to an inner client EXCEPT Get of a
// *corev1.Secret, which it reports NotFound for. It reproduces the operator's
// production cache: internal/cmd/operator/main.go filters the Secret informer
// by an "adopted" label the workshop bearer Secret does not carry, so a cached
// Get of that Secret ALWAYS misses — while the Secret genuinely exists at the
// apiserver (the uncached APIReader).
type secretBlindClient struct {
	client.Client
}

func (c secretBlindClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestReconcile_BearerSecretSurvivesAFilteredCache pins the fifth+ live
// blocker's tail on oap-desktop 2026-09-11: after the RBAC-escalation fix let
// provisioning reach the token step, it wedged at
// TokensReady=False/"secrets ... already exists". The operator's Secret cache
// is label-filtered (main.go ByObject), so the bearer Secret — which carries
// no such label — is never cached; the get-or-create's cached Get always
// returned NotFound, always tried Create, and every reconcile after the first
// died on AlreadyExists. The Workshop could never reach Ready and the sidecar
// never got its identity.
//
// The reconciler already holds an uncached APIReader (used for the namespace
// collision check); token provisioning must read the Secret through it too,
// and tolerate an AlreadyExists on create by reading the existing Secret back.
// Two reconciles through a Secret-blind cached Client (real store behind
// APIReader) must BOTH succeed and reach Ready — the buggy path fails the
// second with AlreadyExists.
func TestReconcile_BearerSecretSurvivesAFilteredCache(t *testing.T) {
	sess := builderSession("builder-blindcache", "e1b2c3d4-e5f6-7890-abcd-ef1234567894")
	ws := sanctionedWorkshop(sess)
	store, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), sess, ws)
	// Cached client is blind to the bearer Secret; the uncached APIReader is
	// the real store, exactly the production split.
	r.APIReader = store
	r.Client = secretBlindClient{Client: store}

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "first provisioning reconcile must not wedge on the bearer Secret")
	got := getWorkshop(t, store, ws)
	require.Equal(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase, "must reach Ready on the first full pass")

	// The regression itself: a SECOND reconcile with the Secret now present but
	// invisible to the cached Client must NOT fail on AlreadyExists.
	_, err = reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "a re-reconcile must recover the existing Secret via the uncached reader, not die on AlreadyExists")
	got = getWorkshop(t, store, ws)
	assert.Equal(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase)
	assert.True(t, testfixtures.HasCondition(got.Status.Conditions, spiceboxv1alpha1.WorkshopConditionTokensReady, metav1.ConditionTrue, spiceboxv1alpha1.ReasonWorkshopProvisioned))
	require.NotNil(t, got.Status.SidecarIdentity)
	assert.Equal(t, spiceboxv1alpha1.WorkshopTokenSecretName(sess.Name), got.Status.SidecarIdentity.TokenSecret)
}

// TestReconcile_ReleasesAWorkshopWhoseBuilderSessionFinished pins the release
// rule: a workshop whose builder session reached a terminal phase is torn down
// like an expired one — but NOT while an install request it made is still
// waiting on an admin, because the install reads the workshop namespace.
//
// Release runs BEFORE provisioning, so a finished session's workshop can never
// re-provision what the teardown is about to remove; the cases below assert
// that by requiring no workshop namespace on a released pass.
func TestReconcile_ReleasesAWorkshopWhoseBuilderSessionFinished(t *testing.T) {
	cases := []struct {
		name         string
		sessionPhase string
		install      *spiceboxv1alpha1.WorkshopInstallStatus
		wantReleased bool
	}{
		{
			name:         "Succeeded with no install request: Released and deleted",
			sessionPhase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			wantReleased: true,
		},
		{
			name:         "Succeeded with an install still Requested: kept, provisions to Ready",
			sessionPhase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			install:      &spiceboxv1alpha1.WorkshopInstallStatus{Phase: spiceboxv1alpha1.WorkshopInstallPhaseRequested},
			wantReleased: false,
		},
		{
			name:         "Succeeded with the install Installed: Released and deleted",
			sessionPhase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			install:      &spiceboxv1alpha1.WorkshopInstallStatus{Phase: spiceboxv1alpha1.WorkshopInstallPhaseInstalled},
			wantReleased: true,
		},
		{
			name:         "still Running: kept, provisions to Ready",
			sessionPhase: spiceboxv1alpha1.AgentSessionPhaseRunning,
			wantReleased: false,
		},
		{
			name:         "Failed: Released and deleted",
			sessionPhase: spiceboxv1alpha1.AgentSessionPhaseFailed,
			wantReleased: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := builderSession("builder-release", "a1b2c3d4-e5f6-7890-abcd-ef1234567899")
			sess.Status.Phase = tc.sessionPhase
			ws := sanctionedWorkshop(sess)
			ws.Status.Install = tc.install
			c, r := newFakeReconciler(t, &fakeTuples{}, tokens.NewRegistry(), sess, ws)

			primeFinalizer(t, r, ws)
			_, err := reconcileWorkshop(t, r, ws)
			require.NoError(t, err)

			got := getWorkshop(t, c, ws)
			var nsList corev1.NamespaceList
			require.NoError(t, c.List(context.Background(), &nsList))

			if !tc.wantReleased {
				assert.Equal(t, spiceboxv1alpha1.WorkshopPhaseReady, got.Status.Phase, "an unfinished workshop keeps provisioning")
				assert.Nil(t, got.DeletionTimestamp, "an unfinished workshop is never deleted")
				assert.Len(t, nsList.Items, 1, "provisioning ran")
				return
			}
			assert.Equal(t, spiceboxv1alpha1.WorkshopPhaseReleased, got.Status.Phase)
			assert.True(t, testfixtures.HasCondition(got.Status.Conditions,
				spiceboxv1alpha1.WorkshopConditionNamespaceReady, metav1.ConditionFalse, spiceboxv1alpha1.ReasonWorkshopSessionFinished),
				"the standing reason is recorded on the workshop, not only in a log")
			assert.NotNil(t, got.DeletionTimestamp, "release deletes the CR so the finalizer teardown runs")
			assert.Empty(t, nsList.Items, "a finished session's workshop must never re-provision on its way out")
		})
	}
}

// A released workshop is deleted, and the NEXT reconcile is the ordinary
// teardown — the same path an expired or hand-deleted workshop takes. Nothing
// about release is a second teardown implementation.
func TestReconcile_AReleasedWorkshopTearsDownOnTheNextPass(t *testing.T) {
	sess := builderSession("builder-release2", "a1b2c3d4-e5f6-7890-abcd-ef1234567898")
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	ws := sanctionedWorkshop(sess)
	// Provisioned before it finished: teardown must reverse the tuple it wrote.
	ws.Status.Namespace = spiceboxv1alpha1.WorkshopNamespaceName(sess.UID)
	ws.Status.Phase = spiceboxv1alpha1.WorkshopPhaseReady
	ft := &fakeTuples{}
	c, r := newFakeReconciler(t, ft, tokens.NewRegistry(), sess, ws)

	primeFinalizer(t, r, ws)
	_, err := reconcileWorkshop(t, r, ws)
	require.NoError(t, err, "release must not error")
	got := getWorkshop(t, c, ws)
	require.NotNil(t, got.DeletionTimestamp)

	_, err = reconcileWorkshop(t, r, got)
	require.NoError(t, err, "the teardown pass must succeed")
	assert.Equal(t, []string{ws.Status.Namespace}, ft.deleted, "teardown reversed the released workshop's tuples")

	var gone spiceboxv1alpha1.Workshop
	err = c.Get(context.Background(), types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}, &gone)
	assert.True(t, apierrors.IsNotFound(err), "the finalizer is released and the CR is gone: %v", err)
}
