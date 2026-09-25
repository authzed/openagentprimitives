//go:build integration

// Wiring-level guard for the agentidentity#platform link, run against a REAL
// SpiceDB (testspicedb) plus a real apiserver (envtest).
//
// The schema-level companion (pkg/authz/spicedb/schema/schema_semantics_integration_test.go)
// proves that agentidentity#update_credential is unsatisfiable without the
// agentidentity:<ns>/<name>#platform@platform:platform tuple. This file proves
// the other half: that something in production actually WRITES it — the
// AgentIdentity reconciler — and that nothing else does.
//
// Both directions are asserted deliberately. A test that only checks "after
// reconcile, the admin is allowed" would still pass if some unrelated write
// happened to create the tuple; the unreconciled control identity is what makes
// the reconciler's write provably load-bearing. Removing the reconciler's
// EnsureAgentIdentityPlatform call MUST turn the first assertion red.
//
//	go test -tags=integration -count=1 ./pkg/controllers/agentidentity/
package agentidentity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// newSpiceDBClient boots a per-test datastore loaded with the canonical schema
// and returns a client bound to it.
func newSpiceDBClient(t *testing.T) *spicedb.Client {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// canUpdateCredential checks agentidentity:<ns>/<name>#update_credential@user:<who>
// fully-consistently. There is no typed Check* helper for this permission yet
// (the consumer arrives in a later task of this slice), so the check is issued
// through the client's raw CheckPermission delegate — the same call the future
// helper will make.
func canUpdateCredential(t *testing.T, c *spicedb.Client, ns, name string, who identity.CanonicalUserID) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		Resource:    &v1.ObjectReference{ObjectType: "agentidentity", ObjectId: ns + "/" + name},
		Permission:  "update_credential",
		Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: who.String()}},
	})
	require.NoError(t, err, "CheckPermission agentidentity:%s/%s#update_credential", ns, name)
	return res.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
}

// TestReconcileWritesPlatformLink_AndWithoutItTheAdminIsRefused is the gate for
// this slice's stated failure mode: a permission nobody can satisfy.
//
// Reconciling an AgentIdentity must write the platform link so a platform admin
// can replace its dead credential; an AgentIdentity that was never reconciled
// must be refused for the SAME admin. The two identities differ in exactly one
// thing — whether Reconcile ran — so the assertion pair isolates the write.
func TestReconcileWritesPlatformLink_AndWithoutItTheAdminIsRefused(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const ns = "default"
	admin := identity.CanonicalFromTrusted("platform-admin-fixture", "test fixture")
	require.NoError(t, spdb.TouchPlatformAdmin(ctx, admin), "TouchPlatformAdmin")

	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	r := &agentidentity.Reconciler{
		Client:         env.Client,
		APIReader:      env.Client,
		SecretReader:   sr,
		PlatformLinker: spdb,
	}

	// Two identities with identical specs. Only "reconciled" is passed through
	// Reconcile; "never-reconciled" is the control.
	for _, name := range []string{"reconciled-bot", "never-reconciled-bot"} {
		require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}), "create AgentIdentity %s", name)
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "reconciled-bot"}})
	require.NoError(t, err, "Reconcile must succeed")

	assert.True(t, canUpdateCredential(t, spdb, ns, "reconciled-bot", admin),
		"after Reconcile, a platform admin MUST be able to replace this identity's credential.\n"+
			"  needed tuple: agentidentity:%s/reconciled-bot#platform@platform:platform\n"+
			"If this is red, the reconciler is no longer writing the platform link and EVERY\n"+
			"credential-update card for an AgentIdentity is unactionable — silently: the schema\n"+
			"still compiles, the card still publishes, the button still renders, the click is refused.",
		ns)

	assert.False(t, canUpdateCredential(t, spdb, ns, "never-reconciled-bot", admin),
		"an AgentIdentity that was never reconciled has no platform link, so the same admin must be\n"+
			"refused — this is what proves the reconciler's write is load-bearing and not incidental")
}

// TestReconcileWritesPlatformLinkEvenWhenTheCredentialIsBroken pins the case the
// whole feature exists for. An AgentIdentity whose Secret is missing (or empty,
// or expired) fails validation and short-circuits to Valid=False — and that is
// PRECISELY the identity a human is being asked to fix. Gating the platform
// link behind the valid path would make update_credential unsatisfiable exactly
// when it is needed, so the link is written before any validation gate.
func TestReconcileWritesPlatformLinkEvenWhenTheCredentialIsBroken(t *testing.T) {
	env := testenv.Shared(t)
	spdb := newSpiceDBClient(t)
	ctx := context.Background()

	const (
		ns   = "default"
		name = "dead-credential-bot"
	)
	admin := identity.CanonicalFromTrusted("platform-admin-fixture", "test fixture")
	require.NoError(t, spdb.TouchPlatformAdmin(ctx, admin), "TouchPlatformAdmin")

	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	r := &agentidentity.Reconciler{
		Client:         env.Client,
		APIReader:      env.Client,
		SecretReader:   sr,
		PlatformLinker: spdb,
	}

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "bot-token", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "does-not-exist", Key: "token"},
				},
			}},
		},
	}), "create AgentIdentity with a missing Secret")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	require.NoError(t, err, "Reconcile must succeed (a missing Secret is a status outcome, not a reconcile error)")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got), "Get after Reconcile")
	require.True(t, hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSecretMissing),
		"precondition: this identity must have short-circuited to Valid=False/SecretMissing")

	assert.True(t, canUpdateCredential(t, spdb, ns, name, admin),
		"a platform admin MUST be able to replace the credential of an INVALID identity — that is the\n"+
			"entire point of the permission. If this is red, the link write has been moved behind a\n"+
			"validation gate and only healthy identities are fixable.")
}

// TestReconcileWithoutAPlatformLinkStillReconciles covers the no-SpiceDB
// fixture path: a nil PlatformLinker must not panic and must not block status
// convergence. The typed-nil hazard this repo has a production incident from
// makes the nil case worth an explicit test — `PlatformLinker` is an interface
// field, and a nil *spicedb.Client assigned into it would be a NON-nil
// interface that panics here rather than taking the nil branch.
func TestReconcileWithoutAPlatformLinkStillReconciles(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	const ns = "default"
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bot-creds", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("not-a-real-token")},
	}), "create Secret")

	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	r := &agentidentity.Reconciler{Client: env.Client, APIReader: env.Client, SecretReader: sr} // PlatformLinker deliberately unset

	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "unlinked-bot", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "bot-token", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "bot-creds", Key: "token"},
				},
			}},
		},
	}), "create AgentIdentity")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "unlinked-bot"}})
	require.NoError(t, err, "a nil PlatformLinker must be skipped, not panic or error")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "unlinked-bot"}, &got), "Get after Reconcile")
	assert.True(t, hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve),
		"status must still converge without SpiceDB wired")
}

// ---------------------------------------------------------------------------
// A failing link must not take status convergence down with it
// ---------------------------------------------------------------------------

// stubLinker is a PlatformLinker whose behaviour a test picks. It counts calls
// so a test can prove the write was attempted at all.
type stubLinker struct {
	calls int
	fn    func(ns, name string) error
}

func (s *stubLinker) EnsureAgentIdentityPlatform(_ context.Context, ns, name string) error {
	s.calls++
	if s.fn == nil {
		return nil
	}
	return s.fn(ns, name)
}

// linkTestIdentity creates a Secret + a valid AgentIdentity named `name` and
// returns a reconciler wired to linker.
func linkTestIdentity(t *testing.T, env *testenv.Env, name string, linker agentidentity.PlatformLinker) *agentidentity.Reconciler {
	t.Helper()
	ctx := context.Background()
	secretName := name + "-creds"
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("not-a-real-token")},
	}), "create Secret for %s", name)
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "bot-token", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
				},
			}},
		},
	}), "create AgentIdentity %s", name)

	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	return &agentidentity.Reconciler{
		Client: env.Client, APIReader: env.Client, SecretReader: sr, PlatformLinker: linker,
	}
}

func reconcileIdentity(t *testing.T, r *agentidentity.Reconciler, name string) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
	return err
}

func readIdentity(t *testing.T, env *testenv.Env, name string) *spiceboxv1alpha1.AgentIdentity {
	t.Helper()
	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, &got), "Get %s after Reconcile", name)
	return &got
}

// TestPlatformLinkFailureStillPatchesStatus is the fix for putting SpiceDB into
// the hard path of a controller that previously had none.
//
// The link write returns its error (deliberate: a permission-GRANTING write
// should be loud), but returning it BEFORE the status patch meant that while
// SpiceDB was unreachable an AgentIdentity never got Valid=True/False at all —
// so every AgentClass gated on that condition stalled. The blast radius of an
// unreachable SpiceDB was the whole identity subsystem, not the one permission
// the link governs.
//
// Mutation sensitivity: move the link error back above the status patch and the
// Valid assertion goes red while the error assertion stays green.
func TestPlatformLinkFailureStillPatchesStatus(t *testing.T) {
	env := testenv.Shared(t)
	linker := &stubLinker{fn: func(string, string) error { return errors.New("spicedb unreachable") }}
	r := linkTestIdentity(t, env, "link-fails-bot", linker)

	err := reconcileIdentity(t, r, "link-fails-bot")
	require.Error(t, err, "a failed permission-granting write must still be LOUD and requeue")
	assert.Equal(t, 1, linker.calls, "the write must be attempted before every validation gate")

	got := readIdentity(t, env, "link-fails-bot")
	assert.True(t, hasCondition(got, spiceboxv1alpha1.AgentIdentityConditionValid,
		metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve),
		"status MUST converge even while SpiceDB is unreachable: an AgentIdentity with no Valid condition "+
			"stalls every AgentClass that references it, which is a far larger outage than the one permission the link grants")
	assert.True(t, hasCondition(got, spiceboxv1alpha1.AgentIdentityConditionPlatformLinked,
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonPlatformLinkFailed),
		"and the CR must SAY the link is missing — otherwise update_credential is unsatisfiable with nothing anywhere admitting it")
}

// TestUnrepresentableIdentityNameIsSurfacedAndNotRetried covers the failure that
// no retry can ever clear.
//
// A Kubernetes object name is a DNS-1123 subdomain, so "support.bot" is legal
// and no CRD pattern forbids it. A SpiceDB object_id is narrower and excludes
// '.', so agentidentity:<ns>/support.bot cannot be written AT ALL. Before this
// fix the reconcile returned that InvalidArgument like any other error and
// requeued forever, wedging the identity with nothing on the CR to explain why.
//
// The linker here is the production composition itself
// (spicedb.AgentIdentityObjectID), which is what the real client calls before
// it issues any RPC — so this pins the real refusal, not a stand-in for it.
func TestUnrepresentableIdentityNameIsSurfacedAndNotRetried(t *testing.T) {
	env := testenv.Shared(t)
	linker := &stubLinker{fn: func(ns, name string) error {
		_, err := spicedb.AgentIdentityObjectID(ns, name)
		return err
	}}
	r := linkTestIdentity(t, env, "support.bot", linker)

	require.NoError(t, reconcileIdentity(t, r, "support.bot"),
		"a name that can NEVER be linked must not requeue forever: no retry can change the answer")

	got := readIdentity(t, env, "support.bot")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionPlatformLinked)
	require.NotNil(t, cond, "the permanent failure must be visible on the CR — it is the ONLY place it is reported")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonUnrepresentableIdentityName, cond.Reason,
		"and distinguished from a transient link failure, because the operator's next action is completely different")
	assert.Contains(t, cond.Message, "support.bot", "the message must name the offending id")

	assert.True(t, hasCondition(got, spiceboxv1alpha1.AgentIdentityConditionValid,
		metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve),
		"the identity itself is fine and every session using it works; only its future REPAIR is impossible")
}

// TestPlatformLinkSuccessRecordsItOnTheCR is the positive control for the two
// above: PlatformLinked must not be a condition that only ever appears in
// failure, or "absent" and "fine" become indistinguishable.
func TestPlatformLinkSuccessRecordsItOnTheCR(t *testing.T) {
	env := testenv.Shared(t)
	r := linkTestIdentity(t, env, "link-ok-bot", &stubLinker{})

	require.NoError(t, reconcileIdentity(t, r, "link-ok-bot"))

	got := readIdentity(t, env, "link-ok-bot")
	assert.True(t, hasCondition(got, spiceboxv1alpha1.AgentIdentityConditionPlatformLinked,
		metav1.ConditionTrue, spiceboxv1alpha1.ReasonPlatformLinked),
		"a linked identity must say so, so that False genuinely means 'not linked' rather than 'not reported'")
}
