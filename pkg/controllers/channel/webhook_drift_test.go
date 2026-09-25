package channel_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github" // register the github kind
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// testAppPEM generates a fresh RSA key, PEM-encoded — CheckWebhookURLDrift
// signs a real App JWT with whatever is in the Secret's private-key, so a
// placeholder string fails before the HTTP call these tests are about.
func testAppPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate test RSA key")
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// githubSecret builds an adopted (pre-labeled) credentials Secret carrying
// the four keys the github kind's RequiredSecretKeys declares.
func githubSecret(t *testing.T, name string) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Data: map[string][]byte{
			"app-id":          []byte("12345"),
			"private-key":     testAppPEM(t),
			"webhook-secret":  []byte("whsec_demo"),
			"installation-id": []byte("67890"),
		},
	}
	adoptguard.WithAdoptedLabel(sec)
	return sec
}

// githubChannel builds a role=input kind=github Channel wired at the given
// credentials Secret. AgentClass is deliberately left unset: the
// WebhookURLDrift check runs unconditionally after validate(), regardless of
// whether the Channel is Valid=True, so these tests do not need a full
// AgentClass fixture to exercise it.
func githubChannel(name, secretName string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "github",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
			GitHub:         &spiceboxv1alpha1.GitHubChannelConfig{AppSlug: "demo-reviewbot"},
		},
	}
}

// TestReconcile_ReportsWebhookURLDriftWithoutRepointingIt is the core case
// from the task brief: the App reports a hook URL on a different host than
// the cluster's configured external base URL. The condition must record the
// mismatch, and the App must never be written back to — silently repointing
// someone's webhook is an outward-facing change that needs a human.
func TestReconcile_ReportsWebhookURLDriftWithoutRepointingIt(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patched = true
		}
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"https://old.demo.test/webhooks/github/default/demo-reviewbot-gh"}}`))
	}))
	t.Cleanup(srv.Close)

	sec := githubSecret(t, "gh-creds")
	ch := githubChannel("demo-reviewbot-gh", "gh-creds")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.ExternalBaseURL = func() string { return "https://new.demo.test" }
	r.GitHubAPIBaseURL = srv.URL

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"},
	})
	require.NoError(t, err, "drift is reported on status, never by failing the reconcile")

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
	require.NotNil(t, cond, "drift must be reported, never silent")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelWebhookURLDrifted, cond.Reason)
	assert.Contains(t, cond.Message, "new.demo.test", "message must name where this cluster actually serves")
	assert.Contains(t, cond.Message, "demo-reviewbot", "message should point at the App's settings page via AppSlug")
	assert.False(t, patched,
		"silently repointing someone's webhook is an outward-facing change that needs a human")
}

// TestReconcile_WebhookURLDrift_NoDriftWhenURLsMatch is the mirror case: the
// App's registered URL already matches where this cluster serves, so the
// condition must be False, not absent and not True.
func TestReconcile_WebhookURLDrift_NoDriftWhenURLsMatch(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	const matching = "https://new.demo.test/webhooks/github/default/demo-reviewbot-gh"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"` + matching + `"}}`))
	}))
	t.Cleanup(srv.Close)

	sec := githubSecret(t, "gh-creds")
	ch := githubChannel("demo-reviewbot-gh", "gh-creds")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.ExternalBaseURL = func() string { return "https://new.demo.test" }
	r.GitHubAPIBaseURL = srv.URL

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
	require.NotNil(t, cond, "the condition must be reported even when there is no drift")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelWebhookURLMatches, cond.Reason)
}

// TestReconcile_WebhookURLDrift_UnreachableProviderIsNotReportedAsNoDrift is
// the negative control for the brief's explicit warning: a failure to REACH
// the App is a different fact from "no drift" and must never read as one.
// The provider here refuses every connection, so CheckWebhookURLDrift must
// return an error, and the controller must set Status=Unknown — never
// silently fall back to False (no drift) or True (drift) with a fabricated
// URL.
func TestReconcile_WebhookURLDrift_UnreachableProviderIsNotReportedAsNoDrift(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := srv.URL
	srv.Close() // closed before use: every dial fails

	sec := githubSecret(t, "gh-creds")
	ch := githubChannel("demo-reviewbot-gh", "gh-creds")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.ExternalBaseURL = func() string { return "https://new.demo.test" }
	r.GitHubAPIBaseURL = unreachableURL

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"},
	})
	require.NoError(t, err, "an unreachable provider must not fail the reconcile either")

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status, "unreachable must be Unknown, never False (no drift) or True (drift)")
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelWebhookProviderUnreachable, cond.Reason)
}

// TestReconcile_WebhookURLDrift_AbsentForKindsWithoutTheCapability proves
// dispatch goes through the registry's optional-interface type-assertion,
// not an if kind=="github" branch: a slack Channel (which does not
// implement channelkinds.WebhookURLDriftChecker) must never get this
// condition set, even with ExternalBaseURL configured.
func TestReconcile_WebhookURLDrift_AbsentForKindsWithoutTheCapability(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "slack-creds"},
		Data:       map[string][]byte{"bot-token": []byte("xoxb-x"), "app-token": []byte("xapp-x")},
	}
	adoptguard.WithAdoptedLabel(sec)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c-slack"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "slack-creds"},
			Slack:          &spiceboxv1alpha1.SlackChannelConfig{Mode: "socket"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.ExternalBaseURL = func() string { return "https://new.demo.test" }

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "c-slack"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "c-slack"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
	assert.Nil(t, cond, "a kind that doesn't implement WebhookURLDriftChecker must never get this condition set")
}

// TestReconcile_WebhookURLDrift_SkippedWhenExternalBaseURLNotConfigured
// guards against manufacturing false drift: with no ExternalBaseURL, the
// expected URL would be a bare path with no host, which would never equal a
// real provider-registered absolute URL — so the check must skip entirely
// rather than report every kind=github Channel in an unconfigured cluster as
// drifted.
func TestReconcile_WebhookURLDrift_SkippedWhenExternalBaseURLNotConfigured(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	sec := githubSecret(t, "gh-creds")
	ch := githubChannel("demo-reviewbot-gh", "gh-creds")

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c) // ExternalBaseURL left nil

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"},
	})
	require.NoError(t, err)

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
	assert.Nil(t, cond, "no ExternalBaseURL means nothing to compare against; must not report drift")
}

// TestReconcile_WebhookURLDrift_ThrottledSkipsTheOutboundCall proves the
// throttle actually gates the network call, not merely the condition write:
// a Channel checked moments ago must not call the provider again before
// webhookURLDriftCheckInterval has elapsed, and the prior condition and
// checked-at timestamp must be left exactly as they were.
func TestReconcile_WebhookURLDrift_ThrottledSkipsTheOutboundCall(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"https://old.demo.test/webhooks/github/default/demo-reviewbot-gh"}}`))
	}))
	t.Cleanup(srv.Close)

	sec := githubSecret(t, "gh-creds")
	ch := githubChannel("demo-reviewbot-gh", "gh-creds")
	// A prior check ran a minute ago and found no drift — the value a
	// throttled tick must leave untouched.
	checkedAt := metav1.NewTime(time.Now().Add(-time.Minute))
	ch.Status.WebhookURLDriftCheckedAt = &checkedAt
	ch.Status.Conditions = []metav1.Condition{{
		Type: spiceboxv1alpha1.ChannelConditionWebhookURLDrift, Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonChannelWebhookURLMatches, LastTransitionTime: metav1.Now(),
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.ExternalBaseURL = func() string { return "https://new.demo.test" }
	r.GitHubAPIBaseURL = srv.URL

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"},
	})
	require.NoError(t, err)

	assert.Equal(t, 0, calls, "a throttled tick must not call the provider at all")
	assert.Greater(t, res.RequeueAfter, time.Duration(0), "a throttled tick must still schedule the next check")
	assert.LessOrEqual(t, res.RequeueAfter, 6*time.Hour, "must requeue for no more than the remaining window")

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"}, &got))
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.ChannelConditionWebhookURLDrift)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "a throttled tick must leave the prior condition untouched")
	require.NotNil(t, got.Status.WebhookURLDriftCheckedAt)
	assert.WithinDuration(t, checkedAt.Time, got.Status.WebhookURLDriftCheckedAt.Time, time.Second,
		"a throttled tick must not advance the checked-at clock")
}

// TestReconcile_WebhookURLDrift_IntervalElapsedRunsAndStampsCheckedAt is the
// mirror of the throttle test: once webhookURLDriftCheckInterval has passed
// since the last check, the next reconcile must call the provider again and
// advance the checked-at timestamp.
func TestReconcile_WebhookURLDrift_IntervalElapsedRunsAndStampsCheckedAt(t *testing.T) {
	// rbacv1: the reconcile stamps the per-Channel webhook-secret Role +
	// RoleBinding (webhookrbac.go), the same as the operator’s own scheme,
	// which registers them via clientgoscheme.
	scheme := testfixtures.NewScheme(t, rbacv1.AddToScheme)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"hook_attributes":{"url":"https://new.demo.test/webhooks/github/default/demo-reviewbot-gh"}}`))
	}))
	t.Cleanup(srv.Close)

	sec := githubSecret(t, "gh-creds")
	ch := githubChannel("demo-reviewbot-gh", "gh-creds")
	staleCheckedAt := metav1.NewTime(time.Now().Add(-7 * time.Hour)) // > the 6h interval
	ch.Status.WebhookURLDriftCheckedAt = &staleCheckedAt

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ch, sec).
		WithStatusSubresource(&spiceboxv1alpha1.Channel{}).Build()

	r := newUnitReconciler(c)
	r.ExternalBaseURL = func() string { return "https://new.demo.test" }
	r.GitHubAPIBaseURL = srv.URL

	before := time.Now()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"},
	})
	require.NoError(t, err)

	assert.Equal(t, 1, calls, "an elapsed interval must run exactly one fresh check")
	assert.Equal(t, 6*time.Hour, res.RequeueAfter, "a check that just ran must schedule the next one a full interval out")

	var got spiceboxv1alpha1.Channel
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "demo-reviewbot-gh"}, &got))
	require.NotNil(t, got.Status.WebhookURLDriftCheckedAt)
	assert.True(t, got.Status.WebhookURLDriftCheckedAt.Time.After(before.Add(-time.Second)),
		"checked-at must advance to (approximately) now, not stay at the stale value")
}
