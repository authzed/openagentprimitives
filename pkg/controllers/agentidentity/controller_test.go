//go:build integration

package agentidentity_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

func newSecretReader(mgr interface {
	GetClient() client.Client
	GetAPIReader() client.Reader
}) *adoptguard.SecretReader {
	return adoptguard.NewSecretReader(mgr.GetClient(), mgr.GetAPIReader(), adoptguard.Warn, func(types.NamespacedName) bool { return false })
}

func startManager(t *testing.T, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if err := (&agentidentity.Reconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		SecretReader: newSecretReader(mgr),
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache sync")
	}
}

// adoptSecret pre-stamps the AdoptedLabel on a secret so the guard permits
// reads via SecretReader in tests that bypass the real adoptkit.AdoptSecret
// server-side-apply path.
func adoptSecret(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &sec),
		"get secret before adoption stamp")
	adoptguard.WithAdoptedLabel(&sec)
	require.NoError(t, c.Update(context.Background(), &sec), "stamp AdoptedLabel on secret")
}

func mustCreate(t *testing.T, c client.Client, o client.Object) {
	t.Helper()
	if err := c.Create(context.Background(), o); err != nil {
		t.Fatalf("create %T %s: %v", o, o.GetName(), err)
	}
}

func eventually(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %v", d)
}

func hasCondition(a *spiceboxv1alpha1.AgentIdentity, condType string, status metav1.ConditionStatus, reason string) bool {
	c := meta.FindStatusCondition(a.Status.Conditions, condType)
	if c == nil {
		return false
	}
	return c.Status == status && (reason == "" || c.Reason == reason)
}

func TestAgent_ValidWhenSecretsResolve(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-creds", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("ghp_xxxxxxxxxxxx")},
	}
	mustCreate(t, env.Client, sec)

	agent := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "gh-pat", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "gh-creds", Key: "token"},
				},
			}},
		},
	}
	mustCreate(t, env.Client, agent)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentIdentity
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(agent), &got); err != nil {
			return false
		}
		return hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve) &&
			len(got.Status.ResolvedCredentials) == 1
	})
}

func TestAgent_InvalidWhenSecretMissing(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	agent := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-secret", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "ghost", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "does-not-exist", Key: "token"},
				},
			}},
		},
	}
	mustCreate(t, env.Client, agent)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentIdentity
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(agent), &got); err != nil {
			return false
		}
		return hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSecretMissing)
	})
}

func TestAgent_InvalidWhenSpecBroken(t *testing.T) {
	env := testenv.Shared(t)
	startManager(t, env)
	ctx := context.Background()

	agent := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-spec", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{Name: "dup", Type: "static", Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "x", Key: "y"}}},
				{Name: "dup", Type: "static", Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "x", Key: "y"}}},
			},
		},
	}
	mustCreate(t, env.Client, agent)

	eventually(t, 5*time.Second, func() bool {
		var got spiceboxv1alpha1.AgentIdentity
		if err := env.Client.Get(ctx, client.ObjectKeyFromObject(agent), &got); err != nil {
			return false
		}
		return hasCondition(&got, spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSpecInvalid)
	})
}

func TestAgentIdentityInvalidWhenSecretValueEmpty(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	r := &agentidentity.Reconciler{Client: env.Client, APIReader: env.Client, SecretReader: sr}

	// Secret with the key present but EMPTY.
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-secret", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("")},
	}), "create empty Secret")
	adoptSecret(t, env.Client, "default", "gh-secret")
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "id-empty", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{{
			Name: "github-token", Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "gh-secret", Key: "token"}},
		}}},
	}
	require.NoError(t, env.Client.Create(ctx, ai), "create AgentIdentity")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "id-empty"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "id-empty"}, &got))
	c := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	require.NotNil(t, c, "Valid condition present")
	assert.Equal(t, metav1.ConditionFalse, c.Status, "empty secret → Valid=False")
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialEmpty, c.Reason)

	// Populate the Secret → re-reconcile → Valid=True.
	got2 := &corev1.Secret{}
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gh-secret"}, got2))
	got2.Data["token"] = []byte("github_pat_realvalue")
	require.NoError(t, env.Client.Update(ctx, got2))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "id-empty"}})
	require.NoError(t, err)
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "id-empty"}, &got))
	c = meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	assert.Equal(t, metav1.ConditionTrue, c.Status, "populated secret → Valid=True")
}

func TestRefresh_EndToEndAgainstHTTPTestServer(t *testing.T) {
	env := testenv.Shared(t)

	// Build a token-endpoint stub.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-at",
			"refresh_token": "fresh-rt",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(tokenSrv.Close)
	refresh.SetHTTPClient(tokenSrv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(http.DefaultClient) })

	// Start a manager with BOTH the validity and refresh reconcilers.
	mgr, err := ctrl.NewManager(env.Cfg, ctrl.Options{
		Scheme:         env.Scheme,
		Metrics:        metricsserver.Options{BindAddress: "0"},
		Controller:     ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		LeaderElection: false,
	})
	require.NoError(t, err, "manager")

	sr := newSecretReader(mgr)
	require.NoError(t, (&agentidentity.Reconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		SecretReader: sr,
	}).SetupWithManager(mgr), "setup validity reconciler")
	require.NoError(t, (&agentidentity.RefreshReconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		SecretReader:     sr,
		DefaultThreshold: 5 * time.Minute,
	}).SetupWithManager(mgr), "setup refresh reconciler")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache sync")

	// Create a Secret whose expires_at is 1m from now (inside the 5m
	// default threshold) so the refresh controller should fire on first
	// reconcile.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data: map[string][]byte{
			"access_token":   []byte("stale-at"),
			"refresh_token":  []byte("stale-rt"),
			"expires_at":     []byte(time.Now().Add(1 * time.Minute).UTC().Format(time.RFC3339)),
			"token_endpoint": []byte(tokenSrv.URL),
			"client_id":      []byte("cid"),
		},
	}
	mustCreate(t, mgr.GetClient(), sec)
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "o", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: "creds"},
				},
			}},
		},
	}
	mustCreate(t, mgr.GetClient(), ai)

	// Poll for stable outcomes: Secret rotated + LastRefreshAt set +
	// Refresh=True. The condition Reason oscillates between
	// RefreshSucceeded (immediately post-refresh) and AllOAuthFresh
	// (after the Secret-watch retrigger sees the fresh expires_at);
	// both are valid post-refresh states, so we don't pin the Reason.
	eventually(t, 30*time.Second, func() bool {
		var gotSec corev1.Secret
		if err := mgr.GetClient().Get(ctx, client.ObjectKey{Namespace: "default", Name: "creds"}, &gotSec); err != nil {
			return false
		}
		if string(gotSec.Data["access_token"]) != "fresh-at" {
			return false
		}
		var gotAI spiceboxv1alpha1.AgentIdentity
		if err := mgr.GetClient().Get(ctx, client.ObjectKey{Namespace: "default", Name: "ai"}, &gotAI); err != nil {
			return false
		}
		if gotAI.Status.LastRefreshAt == nil {
			return false
		}
		return hasCondition(&gotAI, spiceboxv1alpha1.AgentIdentityConditionRefresh,
			metav1.ConditionTrue, "") // any reason
	})

	// Final assertions outside the poll so a failure points to which signal didn't hit.
	var gotSec corev1.Secret
	require.NoError(t, mgr.GetClient().Get(ctx, client.ObjectKey{Namespace: "default", Name: "creds"}, &gotSec), "get secret")
	assert.Equal(t, "fresh-at", string(gotSec.Data["access_token"]), "access_token must rotate")

	var gotAI spiceboxv1alpha1.AgentIdentity
	require.NoError(t, mgr.GetClient().Get(ctx, client.ObjectKey{Namespace: "default", Name: "ai"}, &gotAI), "get agentidentity")
	assert.NotNil(t, gotAI.Status.LastRefreshAt, "lastRefreshAt should be set after a successful refresh")
	_ = meta.FindStatusCondition(gotAI.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionRefresh)
}
