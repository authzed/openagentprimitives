//go:build integration

// Direct-Reconcile envtest coverage for the BrowserToolbox reconciler's
// validity cross-checks: no identity (Valid=True by construction), a missing
// identity, and an identity that exists but carries no directCredentials
// credential. The watch-driven re-validate cases (identity created/deleted
// after the toolbox already reconciled) live in watches_integration_test.go,
// which drives the controller through a real manager instead of calling
// Reconcile directly.
package browsertoolbox_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/browsertoolbox"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// allowlistToolbox builds a minimal, structurally-valid BrowserToolbox:
// Mode=allowlist with one site, optionally naming identity.
func allowlistToolbox(name, identity string) *spiceboxv1alpha1.BrowserToolbox {
	return &spiceboxv1alpha1.BrowserToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode:     "allowlist",
				Sites:    []spiceboxv1alpha1.BrowserSite{{Host: "example.com"}},
				Identity: identity,
			},
		},
	}
}

func reconcileToolbox(t *testing.T, r *browsertoolbox.Reconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
	})
	require.NoError(t, err, "Reconcile")
}

func getToolbox(t *testing.T, env *testenv.Env, name string) *spiceboxv1alpha1.BrowserToolbox {
	t.Helper()
	var got spiceboxv1alpha1.BrowserToolbox
	require.NoError(t, env.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: name}, &got), "Get BrowserToolbox")
	return &got
}

// TestBrowserToolbox_NoIdentity_Valid covers the no-identity path: CEL
// already guarantees structural sanity, so a toolbox with no
// spec.access.identity is Valid=True by construction.
func TestBrowserToolbox_NoIdentity_Valid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &browsertoolbox.Reconciler{Client: env.Client}

	btb := allowlistToolbox("btb-no-identity", "")
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	reconcileToolbox(t, r, "btb-no-identity")

	got := getToolbox(t, env, "btb-no-identity")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonBrowserToolboxValid, cond.Reason, "Valid reason")
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration, "ObservedGeneration stamped")
}

// TestBrowserToolbox_MissingIdentity_Invalid covers spec.access.identity
// naming an AgentIdentity that does not exist in the toolbox's namespace.
func TestBrowserToolbox_MissingIdentity_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &browsertoolbox.Reconciler{Client: env.Client}

	btb := allowlistToolbox("btb-missing-identity", "no-such-identity")
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	reconcileToolbox(t, r, "btb-missing-identity")

	got := getToolbox(t, env, "btb-missing-identity")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonBrowserToolboxIdentityMissing, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "no-such-identity", "message should name the missing identity")
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration, "ObservedGeneration stamped")
}

// TestBrowserToolbox_IdentityWithoutDirectCredentials_Invalid covers an
// AgentIdentity that exists but carries no directCredentials credential --
// the one credential type a BrowserToolbox can bind to.
func TestBrowserToolbox_IdentityWithoutDirectCredentials_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &browsertoolbox.Reconciler{Client: env.Client}

	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-no-directcreds", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "github",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "dummy", Key: "token"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity (no directCredentials)")

	btb := allowlistToolbox("btb-identity-unusable", "id-no-directcreds")
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	reconcileToolbox(t, r, "btb-identity-unusable")

	got := getToolbox(t, env, "btb-identity-unusable")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "id-no-directcreds", "message should name the identity")
	assert.Equal(t, got.Generation, got.Status.ObservedGeneration, "ObservedGeneration stamped")
}

// directCredsIdentity creates an AgentIdentity carrying one directCredentials
// credential and stamps its Valid condition to validStatus with the given
// reason -- the agentidentity reconciler is not running in this test binary,
// so the status is stamped directly, the same convention agentclass's tests
// use for the CRs whose reconcilers they do not run. An empty validStatus
// leaves the identity with no Valid condition at all.
func directCredsIdentity(t *testing.T, env *testenv.Env, name string, validStatus metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentIdentity {
	t.Helper()
	ctx := context.Background()
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "site-login",
				Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    "secret",
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: "dummy", Key: "password"},
				},
			}},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity %s", name)
	if validStatus != "" {
		ai.Status = spiceboxv1alpha1.AgentIdentityStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.AgentIdentityConditionValid, Status: validStatus,
			Reason: reason, LastTransitionTime: metav1.Now(),
		}}}
		require.NoError(t, env.Client.Status().Update(ctx, ai), "stamp AgentIdentity %s Valid=%s", name, validStatus)
	}
	return ai
}

// TestBrowserToolbox_IdentityWithDirectCredentials_Valid covers the happy
// identity path: AgentIdentity exists, carries a directCredentials
// credential, and is itself Valid=True, so the toolbox is Valid=True.
func TestBrowserToolbox_IdentityWithDirectCredentials_Valid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &browsertoolbox.Reconciler{Client: env.Client}

	directCredsIdentity(t, env, "id-with-directcreds", metav1.ConditionTrue, "OK")

	btb := allowlistToolbox("btb-identity-valid", "id-with-directcreds")
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	reconcileToolbox(t, r, "btb-identity-valid")

	got := getToolbox(t, env, "btb-identity-valid")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonBrowserToolboxValid, cond.Reason, "Valid reason")
}

// TestBrowserToolbox_IdentityNotValid_Invalid covers an AgentIdentity that
// carries a directCredentials credential but whose OWN Valid condition is
// False (e.g. the backing Secret is missing) -- shape alone is not enough,
// and the message folds in the identity's condition reason.
func TestBrowserToolbox_IdentityNotValid_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &browsertoolbox.Reconciler{Client: env.Client}

	directCredsIdentity(t, env, "id-not-valid", metav1.ConditionFalse, "SecretMissing")

	btb := allowlistToolbox("btb-identity-not-valid", "id-not-valid")
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	reconcileToolbox(t, r, "btb-identity-not-valid")

	got := getToolbox(t, env, "btb-identity-not-valid")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "id-not-valid", "message should name the identity")
	assert.Contains(t, cond.Message, "SecretMissing", "message should fold in the identity's own condition reason")
}

// TestBrowserToolbox_IdentityNoValidConditionYet_Invalid covers the
// install-ordering shape: the identity exists and is right-shaped but its
// reconciler has not stamped any Valid condition yet. Fail closed as
// Unusable; the AgentIdentity watch re-fires when the condition lands.
func TestBrowserToolbox_IdentityNoValidConditionYet_Invalid(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &browsertoolbox.Reconciler{Client: env.Client}

	directCredsIdentity(t, env, "id-no-condition", "", "")

	btb := allowlistToolbox("btb-identity-no-cond", "id-no-condition")
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	reconcileToolbox(t, r, "btb-identity-no-cond")

	got := getToolbox(t, env, "btb-identity-no-cond")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.BrowserToolboxConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonBrowserToolboxIdentityUnusable, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "no Valid condition yet", "message should say the condition is absent")
}
