//go:build integration

// pkg/controllers/agentsession/passthrough_envtest_test.go
//
// Integration test for the passthrough-identity park → un-park lifecycle.
// Drives a real controller-runtime manager (envtest) through:
//  1. Session parks in AwaitingCredentials, SessionUserIdentity created
//     with MissingCredentials.
//  2. After the user's UserIdentity is created with the required credential,
//     the session un-parks: CredentialsReady=True, SessionUserIdentity
//     Ready=True, passthrough Role/RoleBinding created in
//     agentprimitives-identities namespace.
//
// This test does NOT assert on pod/runner state — those require NATS,
// channelsd, and other infra. The focus is the credential-gate gate.
package agentsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"

	// Registers MCP + CLI + toolspec authkind so RequiredCredentials can
	// resolve MCPServer targets.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/loader"
)

// passthroughNoopRunnerFactory is a minimal RunnerFactory that does nothing.
// The envtest exercise focuses on the credential gate, not pod lifecycle.
type passthroughNoopRunnerFactory struct{}

func (passthroughNoopRunnerFactory) Start(_ context.Context, _ *spiceboxv1alpha1.AgentSession, _ *spiceboxv1alpha1.AgentClass, _ agentsession.StartOpts) error {
	return nil
}
func (passthroughNoopRunnerFactory) Stop(_ context.Context, _ *spiceboxv1alpha1.AgentSession) error {
	return nil
}

// ObservedName returns "" — this fake creates no workload for the
// reconciler to observe.
func (passthroughNoopRunnerFactory) ObservedName(_ *spiceboxv1alpha1.AgentSession) string {
	return ""
}

// startPassthroughManager wires the AgentSession reconciler to a real
// controller-runtime manager and blocks until the cache has synced.
func startPassthroughManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "ctrl.NewManager")

	r := &agentsession.Reconciler{
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		Tokens:        tokens.NewRegistry(),
		Memory:        memory.NewLocal(inmem.NewBackend()),
		RunnerFactory: passthroughNoopRunnerFactory{},
	}
	require.NoError(t, r.SetupWithManager(mgr), "agentsession.SetupWithManager")

	ctx, cancel := context.WithCancel(memory.WithSystemApproval(context.Background(), "test"))
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")
}

// TestPassthrough_ParkAndUnPark exercises the full park → un-park lifecycle
// against a real envtest API server + controller-runtime manager.
func TestPassthrough_ParkAndUnPark(t *testing.T) {
	env := testenv.Shared(t)
	startPassthroughManager(t, env)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// ---- Step 1: set up objects ----------------------------------------

	// agentprimitives-identities namespace (passthrough RBAC lives here).
	// Idempotent: the shared envtest apiserver never deletes namespaces, so a
	// sibling test that also needs this namespace may have created it first.
	identNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace},
	}
	if err := env.Client.Create(ctx, identNS); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create identities namespace")
	}

	// AgentClass: userPassthrough, references MCPServer "linear".
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pass-cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough,
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-opus-4-7",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns:    50,
				MaxTokens:   100000,
				MaxDuration: metav1.Duration{Duration: time.Hour},
			},
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "linear", Ref: "linear"},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass pass-cls")

	// Stamp AgentClass as Valid=True so the session reconciler proceeds past gate 1.
	ac.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentClassConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAllReferencesResolve,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, env.Client.Status().Update(ctx, ac), "stamp AgentClass Valid=True")

	// MCPServer "linear" with a named credential. The CRD requires spec.server.url
	// and an explicit (possibly empty) tools slice; Auth.Credential is the
	// passthrough gate key the session waits for.
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Server: spiceboxv1alpha1.MCPServerServer{URL: "https://linear.example.com/mcp"},
			Tools:  []spiceboxv1alpha1.MCPServerTool{},
			Auth:   spiceboxv1alpha1.MCPServerAuth{Credential: "linear-oauth"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, mcp), "create MCPServer linear")

	// AgentSession carrying the starter annotation.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pt-sess",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:envtest@example.com",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "pass-cls",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "help me"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession pt-sess")

	// ---- Step 2: poll until session is parked ---------------------------
	// The controller should reconcile, discover the missing UserIdentity, and
	// park the session in AwaitingCredentials with a SessionUserIdentity listing
	// "linear-oauth" as a missing credential.

	testfixtures.Eventually(t, 30*time.Second, func() bool {
		var gotSess spiceboxv1alpha1.AgentSession
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &gotSess); err != nil {
			return false
		}
		if gotSess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
			return false
		}
		credCond := meta.FindStatusCondition(gotSess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
		if credCond == nil || credCond.Status != metav1.ConditionFalse {
			return false
		}
		var suid spiceboxv1alpha1.SessionUserIdentity
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pt-sess"}, &suid); err != nil {
			return false
		}
		for _, m := range suid.Status.MissingCredentials {
			if m == "linear-oauth" {
				return true
			}
		}
		return false
	})

	// Confirm assertions outside the poll so a failure names the missing signal.
	var parkedSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &parkedSess),
		"get AgentSession after parking")
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, parkedSess.Status.Phase,
		"session phase")
	credCond := meta.FindStatusCondition(parkedSess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
	require.NotNil(t, credCond, "CredentialsReady condition must be set")
	assert.Equal(t, metav1.ConditionFalse, credCond.Status, "CredentialsReady.Status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAwaitingUserCredentials, credCond.Reason, "CredentialsReady.Reason")

	var parkedSUID spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pt-sess"}, &parkedSUID),
		"get SessionUserIdentity after parking")
	assert.Contains(t, parkedSUID.Status.MissingCredentials, "linear-oauth",
		"SessionUserIdentity.MissingCredentials")
	assert.NotNil(t, parkedSUID.Status.ParkedAt, "SessionUserIdentity.ParkedAt should be stamped")

	// ---- Step 3: resolve the gap ----------------------------------------
	// Create the master Secret in IdentitiesNamespace, then create the
	// UserIdentity referencing it.

	masterSecretName := useridentity.NameForSubject("user:envtest@example.com") + "-linear-oauth"
	masterSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      masterSecretName,
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		},
		Data: map[string][]byte{"token": []byte("tok-linear-envtest")},
	}
	require.NoError(t, env.Client.Create(ctx, masterSecret), "create master Secret in identities namespace")

	uiName := useridentity.NameForSubject("user:envtest@example.com")
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: uiName},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:envtest@example.com",
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "linear-oauth",
					Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{
							Name: masterSecretName,
							Key:  "token",
						},
					},
				},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, ui), "create UserIdentity for envtest@example.com")

	// ---- Step 4: poll until session un-parks ----------------------------
	// The controller watches UserIdentity and re-enqueues parked sessions
	// (sessionsForUserIdentityChange). Once it reconciles, it should find all
	// credentials present, write RBAC, and set CredentialsReady=True.

	testfixtures.Eventually(t, 30*time.Second, func() bool {
		var gotSess spiceboxv1alpha1.AgentSession
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &gotSess); err != nil {
			return false
		}
		credCond := meta.FindStatusCondition(gotSess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
		if credCond == nil || credCond.Status != metav1.ConditionTrue {
			return false
		}
		var suid spiceboxv1alpha1.SessionUserIdentity
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pt-sess"}, &suid); err != nil {
			return false
		}
		suidReady := meta.FindStatusCondition(suid.Status.Conditions, spiceboxv1alpha1.SessionUserIdentityConditionReady)
		if suidReady == nil || suidReady.Status != metav1.ConditionTrue {
			return false
		}
		// The required credential is type=static → it is projected into a
		// per-session Secret in the SESSION namespace, with a get-only reader
		// Role (no identities-namespace master Role for an all-static session).
		var projected corev1.Secret
		if err := env.Client.Get(ctx, client.ObjectKey{
			Namespace: "default",
			Name:      spiceboxv1alpha1.PassthroughCredentialSecretName(gotSess.Name),
		}, &projected); err != nil {
			return false
		}
		readerRole := gotSess.Name + "-passthrough-cred-reader"
		var role rbacv1.Role
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: readerRole}, &role); err != nil {
			return false
		}
		var rb rbacv1.RoleBinding
		if err := env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: readerRole}, &rb); err != nil {
			return false
		}
		return len(suid.Status.MissingCredentials) == 0
	})

	// Final assertions so test output names the missing signals clearly.
	// Note: the AgentSession phase is not reset from AwaitingCredentials by the
	// operator itself — the runner process writes the next phase (Pending/Running).
	// The definitive signal the gate has cleared is CredentialsReady=True.
	var unparkedSess spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &unparkedSess),
		"get AgentSession after un-parking")
	credCond = meta.FindStatusCondition(unparkedSess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
	require.NotNil(t, credCond, "CredentialsReady condition must still exist after un-parking")
	assert.Equal(t, metav1.ConditionTrue, credCond.Status, "CredentialsReady.Status after un-park")

	var unparkedSUID spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pt-sess"}, &unparkedSUID),
		"get SessionUserIdentity after un-parking")
	assert.Empty(t, unparkedSUID.Status.MissingCredentials,
		"SessionUserIdentity.MissingCredentials should be empty after credential is linked")
	suidReadyCond := meta.FindStatusCondition(unparkedSUID.Status.Conditions, spiceboxv1alpha1.SessionUserIdentityConditionReady)
	require.NotNil(t, suidReadyCond, "SessionUserIdentity Ready condition must be set")
	assert.Equal(t, metav1.ConditionTrue, suidReadyCond.Status, "SessionUserIdentity Ready.Status")

	// The required credential is type=static, so its VALUE is projected into a
	// per-session Secret in the SESSION namespace (not the master, cross-namespace),
	// guarded by a get-only reader Role/RoleBinding. No identities-namespace master
	// Role is created for an all-static session.
	var projected corev1.Secret
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{
		Namespace: "default",
		Name:      spiceboxv1alpha1.PassthroughCredentialSecretName(unparkedSess.Name),
	}, &projected), "per-session credential Secret must exist in the session namespace")
	val := projected.StringData["linear-oauth"]
	if val == "" {
		val = string(projected.Data["linear-oauth"])
	}
	assert.Equal(t, "tok-linear-envtest", val, "projected value keyed by credential name")

	readerRole := unparkedSess.Name + "-passthrough-cred-reader"
	var role rbacv1.Role
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: readerRole}, &role),
		"per-session credential reader Role must exist in the session namespace")
	assert.Equal(t, []string{"get"}, role.Rules[0].Verbs, "reader Role is get-only")
	var rb rbacv1.RoleBinding
	require.NoError(t, env.Client.Get(ctx, client.ObjectKey{Namespace: "default", Name: readerRole}, &rb),
		"per-session credential reader RoleBinding must exist in the session namespace")

	// No identities-namespace master Role for an all-static session.
	var masterRole rbacv1.Role
	mErr := env.Client.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      agentsession.PassthroughRoleName(&unparkedSess),
	}, &masterRole)
	assert.True(t, apierrors.IsNotFound(mErr),
		"no identities-namespace master Role should be created when every credential is type=static")
}
