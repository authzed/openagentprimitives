//go:build integration

// pkg/controllers/agentsession/browserstate_envtest_test.go
//
// Integration tests for Phase C Task C9's `persistent` browser state Secret:
//   - the EFFECTIVE-state snapshot default (C-R5): an unset state resolves to
//     persistent iff an identity is bound, else ephemeral; an explicit value is
//     honored.
//   - the per-session state Secret is created EMPTY for a persistent toolbox,
//     is ABSENT for an ephemeral one, and SURVIVES reap (so wake re-injects).
//   - the runner Role gains secrets get;update pinned to EXACTLY that state
//     Secret for a persistent toolbox, and NO such grant for an ephemeral one.
//
// Shares the browser fixtures from browserpod_envtest_test.go (newBrowserReconciler,
// browserToolbox, classWithBrowsers, setBrowserPodReady) and the identity fixtures
// from browsercatalog_envtest_test.go (browserLoginIdentity, browserLoginSecret),
// including that file's blank import of the real `secret` directcred source.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// runnerRoleHasSecretVerb reports whether the session's runner Role grants `verb`
// on the named Secret via a resourceNames-pinned core/secrets rule.
func runnerRoleHasSecretVerb(t *testing.T, ctx context.Context, env *testenv.Env, sessName, secretName, verb string) bool {
	t.Helper()
	var role rbacv1.Role
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: sessName + "-runner"}, &role),
		"get runner Role")
	for _, r := range role.Rules {
		if !containsRuleStr(r.APIGroups, "") || !containsRuleStr(r.Resources, "secrets") {
			continue
		}
		if containsRuleStr(r.ResourceNames, secretName) && containsRuleStr(r.Verbs, verb) {
			return true
		}
	}
	return false
}

func containsRuleStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestPersistentBrowserStateSecretLifecycle proves the full persistent path for a
// toolbox with a bound identity and an UNSET state: the snapshot resolves it to
// persistent, the state Secret is created empty, the runner Role gains get;update
// pinned to it, and the Secret (with runner-written content) survives reap.
func TestPersistentBrowserStateSecretLifecycle(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	sec := browserLoginSecret("btb-state-logins", "aa.com", "irrelevant-secret")
	require.NoError(t, env.Client.Create(ctx, sec), "create login Secret")
	id := browserLoginIdentity("btb-state-id", "btb-state-logins")
	require.NoError(t, env.Client.Create(ctx, id), "create AgentIdentity")

	// UNSET state + a bound identity → effective persistent (C-R5 behavior a).
	tb := browserToolbox("btb-state", "aa.com")
	tb.Spec.State = "" // explicitly unset; the snapshot defaults it.
	tb.Spec.Access.Identity = "btb-state-id"
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-state",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-state"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-state", "ac-browser-state")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	reconcileToWork(t, ctx, r, "s-browser-state")

	// (a) Snapshot resolved the effective state to persistent.
	rt := firstBrowser(t, ctx, env, "s-browser-state")
	assert.Equal(t, spiceboxv1alpha1.BrowserStatePersistent, rt.Spec.State,
		"an identity-bound, state-unset toolbox must resolve to persistent")

	// State Secret created, EMPTY.
	stateName := agentsession.BrowserStateSecretName(sess, "btb-state")
	var stateSec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: stateName}, &stateSec),
		"persistent toolbox must have an empty state Secret")
	assert.Empty(t, stateSec.Data["state"],
		"operator creates the state Secret with no content; the runner is the sole writer")

	// RBAC: runner Role grants get;update on exactly the state Secret.
	assert.True(t, runnerRoleHasSecretVerb(t, ctx, env, "s-browser-state", stateName, "get"),
		"runner Role must grant secrets/get on the state Secret")
	assert.True(t, runnerRoleHasSecretVerb(t, ctx, env, "s-browser-state", stateName, "update"),
		"runner Role must grant secrets/update on the state Secret")

	// Simulate the runner writing captured state, then prove reap keeps it.
	stateSec.Data = map[string][]byte{"state": []byte(`{"cookies":[{"name":"sid","value":"x"}]}`)}
	require.NoError(t, env.Client.Update(ctx, &stateSec), "simulate runner writing state")

	// Drive to reap: an archive-swept Succeeded session reaps its pods immediately.
	key := types.NamespacedName{Namespace: "default", Name: "s-browser-state"}
	var drive spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &drive))
	drive.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	now := metav1.Now()
	drive.Status.FinishedAt = &now
	meta.SetStatusCondition(&drive.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.AgentSessionConditionIdle,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived,
	})
	require.NoError(t, env.Client.Status().Update(ctx, &drive), "mark session archive-swept Succeeded")
	reconcileToWork(t, ctx, r, "s-browser-state")

	// The browser pod is reaped, but the state Secret — and its content — survive.
	podName := agentsession.BrowserPodName(sess, "btb-state")
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &corev1.Pod{})
	assert.True(t, apierrors.IsNotFound(err), "browser pod must be reaped, got err=%v", err)

	var afterReap corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: stateName}, &afterReap),
		"state Secret must SURVIVE reap so wake can re-inject")
	assert.Equal(t, `{"cookies":[{"name":"sid","value":"x"}]}`,
		string(afterReap.Data["state"]),
		"reap must not wipe the runner-written state content")
}

// TestEphemeralBrowserHasNoStateSecret proves an ephemeral toolbox (no identity,
// unset state → effective ephemeral, C-R5 behavior b) gets NO state Secret and NO
// secrets-write grant on the runner Role.
func TestEphemeralBrowserHasNoStateSecret(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	tb := browserToolbox("btb-eph", "example.com") // no identity, state "ephemeral"
	tb.Spec.State = ""                             // unset → defaults to ephemeral (no identity).
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-eph",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-eph"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-eph", "ac-browser-eph")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	reconcileToWork(t, ctx, r, "s-browser-eph")

	rt := firstBrowser(t, ctx, env, "s-browser-eph")
	assert.Equal(t, spiceboxv1alpha1.BrowserStateEphemeral, rt.Spec.State,
		"an identity-less, state-unset toolbox must resolve to ephemeral (preserves Phase A)")

	stateName := agentsession.BrowserStateSecretName(sess, "btb-eph")
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: stateName}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "ephemeral toolbox must have NO state Secret, got err=%v", err)

	assert.False(t, runnerRoleHasSecretVerb(t, ctx, env, "s-browser-eph", stateName, "update"),
		"an ephemeral toolbox must NOT grant any secrets/update on a state Secret")
}

// TestExplicitEphemeralWithIdentityHasNoStateSecret proves C-R5 behavior (d): an
// EXPLICIT ephemeral is honored even with an identity bound — no state Secret, no
// write grant, the stricter posture a credentialed toolbox may opt into.
func TestExplicitEphemeralWithIdentityHasNoStateSecret(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	sec := browserLoginSecret("btb-xe-logins", "aa.com", "irrelevant")
	require.NoError(t, env.Client.Create(ctx, sec), "create login Secret")
	id := browserLoginIdentity("btb-xe-id", "btb-xe-logins")
	require.NoError(t, env.Client.Create(ctx, id), "create AgentIdentity")

	tb := browserToolbox("btb-xe", "aa.com")
	tb.Spec.State = spiceboxv1alpha1.BrowserStateEphemeral // explicit ephemeral
	tb.Spec.Access.Identity = "btb-xe-id"
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-xe",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-xe"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-xe", "ac-browser-xe")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	reconcileToWork(t, ctx, r, "s-browser-xe")

	rt := firstBrowser(t, ctx, env, "s-browser-xe")
	assert.Equal(t, spiceboxv1alpha1.BrowserStateEphemeral, rt.Spec.State,
		"an explicit ephemeral must be honored even with an identity bound")

	stateName := agentsession.BrowserStateSecretName(sess, "btb-xe")
	err := env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: stateName}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "explicit-ephemeral toolbox must have NO state Secret, got err=%v", err)
	assert.False(t, runnerRoleHasSecretVerb(t, ctx, env, "s-browser-xe", stateName, "update"),
		"explicit-ephemeral toolbox must NOT grant secrets/update on a state Secret")
}
