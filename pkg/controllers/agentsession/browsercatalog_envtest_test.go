//go:build integration

// pkg/controllers/agentsession/browsercatalog_envtest_test.go
//
// Integration tests for Phase B Task 8: listing a bound identity's
// directCredentials login catalog and its identity-derived sites onto
// AgentSession status. These use the REAL `secret` directcred.Source
// (host-keyed login Secret), which is envtest-friendly: its List is a pure
// function of the Secret's data with no network call, so no fake is needed.
//
// What is pinned here:
//   - catalog + identity sites land: effectiveSites = explicit ∪ identity with
//     the right Source values, canonical; catalog sorted by id; IdentityName
//     stamped; the gate policy ConfigMap carries the identity hosts too.
//   - the listing cadence: carried forward on a steady-state reconcile (a
//     changed login Secret with no wake does NOT change the catalog — proof the
//     Source was not re-listed), re-listed on a wake.
//   - no credential VALUE ever reaches status (whole-status JSON NotContains the
//     fixture password).
//   - a wake whose identity has been deleted fails closed with BrowserBootFailed
//     naming the identity.
package agentsession_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"

	// Register the real "secret" directcred.Source in THIS test binary — a test
	// binary never links the operator's blank imports. No network: its List is
	// a pure function of the host-keyed Secret's data.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/directcred/secretsource"
)

// browserPassSentinel is a value no catalog or effectiveSites entry may ever
// carry. It is the password behind one login in the fixture Secret; the
// sentinel test asserts the whole marshaled status never contains it.
const browserPassSentinel = "p4ssw0rd-SENTINEL-must-never-leak"

// browserLoginIdentity builds an AgentIdentity carrying one secret-backed
// directCredentials credential pointing at the host-keyed login Secret.
func browserLoginIdentity(name, loginSecret string) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "logins", Type: "directCredentials",
				DirectCredentials: &spiceboxv1alpha1.DirectCredentialsSource{
					Source:    directcred.SourceSecret,
					SecretRef: spiceboxv1alpha1.DirectCredentialSecretRef{Name: loginSecret},
				},
			}},
		},
	}
}

// browserLoginSecret builds a host-keyed login Secret (the `secret` backend's
// layout: "<host>.username" / "<host>.password") for one host, with the given
// password.
func browserLoginSecret(name, host, pass string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Data: map[string][]byte{
			host + ".username": []byte("alice@example.com"),
			host + ".password": []byte(pass),
		},
	}
}

// browserToolboxBoundTo clones browserToolbox and binds it to an identity with
// identitySites on, so the identity's catalog hosts fold into the allowlist.
func browserToolboxBoundTo(name, identity string, explicitHosts ...string) *spiceboxv1alpha1.BrowserToolbox {
	tb := browserToolbox(name, explicitHosts...)
	tb.Spec.Access.Identity = identity
	tb.Spec.Access.IdentitySites = true
	return tb
}

// firstBrowser returns the single resolved browser toolbox from a session's
// status, failing if absent.
func firstBrowser(t *testing.T, ctx context.Context, env *testenv.Env, name string) spiceboxv1alpha1.ResolvedBrowserToolbox {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &got), "get session")
	require.Len(t, got.Status.ResolvedBrowserToolboxes, 1, "one resolved browser toolbox in status")
	return got.Status.ResolvedBrowserToolboxes[0]
}

// TestBrowserCatalogAndIdentitySitesLand proves the happy path: the identity's
// login catalog is listed onto status (sorted, value-free), identityName is
// stamped, effectiveSites is explicit ∪ identity with the right Source values,
// and the gate policy ConfigMap carries the identity hosts the §5.2 SSO
// reachability rides on.
func TestBrowserCatalogAndIdentitySitesLand(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	// Two hosts out of canonical order in the Secret map so the sort is real.
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "btb-cat-logins", Namespace: "default"},
		Data: map[string][]byte{
			"zeta.example.com.username":  []byte("alice@example.com"),
			"zeta.example.com.password":  []byte(browserPassSentinel),
			"alpha.example.com.username": []byte("bob@example.com"),
			"alpha.example.com.password": []byte("another-secret"),
		},
	}
	require.NoError(t, env.Client.Create(ctx, sec), "create login Secret")

	id := browserLoginIdentity("btb-cat-id", "btb-cat-logins")
	require.NoError(t, env.Client.Create(ctx, id), "create AgentIdentity")

	tb := browserToolboxBoundTo("btb-cat", "btb-cat-id", "explicit.example.org")
	require.NoError(t, env.Client.Create(ctx, tb), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-cat",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-cat"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-cat", "ac-browser-cat")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	reconcileToWork(t, ctx, r, "s-browser-cat")

	rt := firstBrowser(t, ctx, env, "s-browser-cat")

	// IdentityName stamped.
	assert.Equal(t, "btb-cat-id", rt.IdentityName, "the bound identity is recorded for Phase C's filler")

	// Catalog: one ref per host, sorted by id, value-free.
	require.Len(t, rt.Catalog, 2, "one catalog entry per host")
	assert.Equal(t, "alpha.example.com", rt.Catalog[0].ID, "catalog sorted by id (alpha before zeta)")
	assert.Equal(t, "zeta.example.com", rt.Catalog[1].ID)
	assert.Equal(t, "alpha.example.com", rt.Catalog[0].Label, "secret backend labels by host")
	require.Len(t, rt.Catalog[0].Hosts, 1)
	assert.Equal(t, "alpha.example.com", rt.Catalog[0].Hosts[0].Host)
	assert.False(t, rt.Catalog[0].Hosts[0].Exact, "secret backend grants per-host (non-exact)")

	// effectiveSites = explicit (first) ∪ identity (appended), canonical, Source set.
	require.Len(t, rt.EffectiveSites, 3, "one explicit + two identity hosts")
	assert.Equal(t, spiceboxv1alpha1.BrowserEffectiveSite{Host: "explicit.example.org", Source: "explicit"}, rt.EffectiveSites[0])
	assert.Equal(t, spiceboxv1alpha1.BrowserEffectiveSite{Host: "alpha.example.com", Source: "identity"}, rt.EffectiveSites[1])
	assert.Equal(t, spiceboxv1alpha1.BrowserEffectiveSite{Host: "zeta.example.com", Source: "identity"}, rt.EffectiveSites[2])

	// The gate policy ConfigMap carries the identity hosts too (what §5.2 SSO
	// reachability depends on).
	var cm corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: agentsession.BrowserPolicyConfigMapName(sess, "btb-cat")}, &cm),
		"browser policy ConfigMap must exist")
	var doc browserpolicy.PolicyDoc
	require.NoError(t, json.Unmarshal([]byte(cm.Data["policy.json"]), &doc), "policy ConfigMap holds valid PolicyDoc JSON")
	gateHosts := map[string]bool{}
	for _, s := range doc.Sites {
		gateHosts[s.Host] = true
	}
	assert.True(t, gateHosts["explicit.example.org"], "gate doc carries the explicit host")
	assert.True(t, gateHosts["alpha.example.com"], "gate doc carries identity host alpha")
	assert.True(t, gateHosts["zeta.example.com"], "gate doc carries identity host zeta")
}

// TestBrowserStatusCarriesNoCredentialValue marshals the WHOLE AgentSession
// status to JSON and asserts it never contains the fixture password — the
// catalog is {id,label,hosts} only, and no value rests on status (spec §1.5,
// §4.4).
func TestBrowserStatusCarriesNoCredentialValue(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, browserLoginSecret("btb-leak-logins", "login.example.com", browserPassSentinel)), "create login Secret")
	require.NoError(t, env.Client.Create(ctx, browserLoginIdentity("btb-leak-id", "btb-leak-logins")), "create AgentIdentity")
	require.NoError(t, env.Client.Create(ctx, browserToolboxBoundTo("btb-leak", "btb-leak-id")), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-leak",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-leak"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-leak", "ac-browser-leak")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	reconcileToWork(t, ctx, r, "s-browser-leak")

	// The catalog must have landed (so we are actually testing a populated status).
	rt := firstBrowser(t, ctx, env, "s-browser-leak")
	require.Len(t, rt.Catalog, 1, "catalog listed, so the sentinel check is meaningful")

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "s-browser-leak"}, &got), "get session")
	raw, err := json.Marshal(got.Status)
	require.NoError(t, err, "marshal whole session status")
	assert.NotContains(t, string(raw), browserPassSentinel,
		"no credential value may rest on AgentSession status; the catalog carries only {id,label,hosts}")
}

// TestBrowserCatalogCarryForwardAndWakeRelist pins the listing cadence without a
// call counter: ten steady-state reconciles leave the session/pod/CM
// resourceVersions AND the catalog unchanged (proof of no churn and no
// re-list); replacing the login Secret WITHOUT a wake still does not change the
// catalog (proof the Source was not re-listed); only a wake re-lists it against
// the new Secret.
func TestBrowserCatalogCarryForwardAndWakeRelist(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	loginKey := types.NamespacedName{Namespace: "default", Name: "btb-cf-logins"}
	require.NoError(t, env.Client.Create(ctx, browserLoginSecret("btb-cf-logins", "app1.example.com", browserPassSentinel)), "create login Secret")
	require.NoError(t, env.Client.Create(ctx, browserLoginIdentity("btb-cf-id", "btb-cf-logins")), "create AgentIdentity")
	require.NoError(t, env.Client.Create(ctx, browserToolboxBoundTo("btb-cf", "btb-cf-id")), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-cf",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-cf"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-cf", "ac-browser-cf")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	key := types.NamespacedName{Namespace: "default", Name: "s-browser-cf"}
	reconcileToWork(t, ctx, r, "s-browser-cf")

	// Initial list.
	rt := firstBrowser(t, ctx, env, "s-browser-cf")
	require.Len(t, rt.Catalog, 1)
	assert.Equal(t, "app1.example.com", rt.Catalog[0].ID, "first list reflects app1")

	podName := agentsession.BrowserPodName(sess, "btb-cf")
	cmName := agentsession.BrowserPolicyConfigMapName(sess, "btb-cf")
	rv := func(obj interface{ GetResourceVersion() string }) string { return obj.GetResourceVersion() }
	getRVs := func() (string, string, string) {
		var s spiceboxv1alpha1.AgentSession
		require.NoError(t, env.Client.Get(ctx, key, &s))
		var pod corev1.Pod
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: podName}, &pod))
		var cm corev1.ConfigMap
		require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: cmName}, &cm))
		return rv(&s), rv(&pod), rv(&cm)
	}
	sRV, pRV, cmRV := getRVs()

	// Ten steady-state reconciles: no churn, no re-list.
	for i := 0; i < 10; i++ {
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}
	gotS, gotP, gotCM := getRVs()
	assert.Equal(t, sRV, gotS, "session must not be rewritten across 10 steady-state reconciles")
	assert.Equal(t, pRV, gotP, "browser pod must not be rewritten")
	assert.Equal(t, cmRV, gotCM, "policy ConfigMap must not be rewritten")
	after := firstBrowser(t, ctx, env, "s-browser-cf")
	assert.Equal(t, rt.Catalog, after.Catalog, "catalog carried forward unchanged (no re-list)")

	// Replace the login Secret with a DIFFERENT host, no wake. The secret
	// backend has no token key, so the catalog fingerprint is unchanged and the
	// Source is NOT re-listed — the catalog must still name app1.
	var liveSec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, loginKey, &liveSec), "get login Secret to replace")
	liveSec.Data = map[string][]byte{
		"app2.example.com.username": []byte("alice@example.com"),
		"app2.example.com.password": []byte("rotated-secret"),
	}
	require.NoError(t, env.Client.Update(ctx, &liveSec), "replace login Secret data (no wake)")
	reconcileToWork(t, ctx, r, "s-browser-cf")
	noWake := firstBrowser(t, ctx, env, "s-browser-cf")
	require.Len(t, noWake.Catalog, 1)
	assert.Equal(t, "app1.example.com", noWake.Catalog[0].ID,
		"a changed login Secret WITHOUT a wake must not trigger a re-list (carry-forward)")

	// Drive to the reap path (archive-swept Succeeded), reaping the browser pod
	// but keeping both Secrets, then wake: the wake re-lists against the new
	// Secret and the catalog now names app2.
	var drive spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &drive), "get session to drive to Succeeded")
	drive.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	now := metav1.Now()
	drive.Status.FinishedAt = &now
	meta.SetStatusCondition(&drive.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.AgentSessionConditionIdle,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived,
	})
	require.NoError(t, env.Client.Status().Update(ctx, &drive), "mark session archive-swept Succeeded")
	reconcileToWork(t, ctx, r, "s-browser-cf")

	var waking spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &waking), "get session to wake")
	if waking.Annotations == nil {
		waking.Annotations = map[string]string{}
	}
	waking.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &waking), "stamp wake annotation")
	reconcileToWork(t, ctx, r, "s-browser-cf")

	woke := firstBrowser(t, ctx, env, "s-browser-cf")
	require.Len(t, woke.Catalog, 1)
	assert.Equal(t, "app2.example.com", woke.Catalog[0].ID, "a wake re-lists against the current Secret (app2)")
}

// TestBrowserWakeWithDeletedIdentityFailsClosed proves the fail-closed contract:
// if the bound identity is gone when a wake re-lists, the session fails with
// BrowserBootFailed naming the identity — never a silently-empty catalog.
func TestBrowserWakeWithDeletedIdentityFailsClosed(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := newBrowserReconciler(t, env)

	require.NoError(t, env.Client.Create(ctx, browserLoginSecret("btb-del-logins", "app.example.com", browserPassSentinel)), "create login Secret")
	id := browserLoginIdentity("btb-del-id", "btb-del-logins")
	require.NoError(t, env.Client.Create(ctx, id), "create AgentIdentity")
	require.NoError(t, env.Client.Create(ctx, browserToolboxBoundTo("btb-del", "btb-del-id")), "create BrowserToolbox")

	ac := classWithBrowsers("ac-browser-del",
		spiceboxv1alpha1.AgentClassBrowserToolboxRef{Name: "web", Ref: "btb-del"})
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	markValid(t, env, ac)

	sess := validSession("s-browser-del", "ac-browser-del")
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")
	key := types.NamespacedName{Namespace: "default", Name: "s-browser-del"}
	reconcileToWork(t, ctx, r, "s-browser-del")
	require.Len(t, firstBrowser(t, ctx, env, "s-browser-del").Catalog, 1, "catalog listed before the identity is deleted")

	// Park the session archive-swept so the wake path runs, then delete the
	// identity and wake.
	var drive spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &drive), "get session to drive to Succeeded")
	drive.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
	now := metav1.Now()
	drive.Status.FinishedAt = &now
	meta.SetStatusCondition(&drive.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.AgentSessionConditionIdle,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonAgentSessionArchived,
	})
	require.NoError(t, env.Client.Status().Update(ctx, &drive), "mark session archive-swept Succeeded")
	reconcileToWork(t, ctx, r, "s-browser-del")

	require.NoError(t, env.Client.Delete(ctx, id), "delete the bound AgentIdentity")

	var waking spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &waking), "get session to wake")
	if waking.Annotations == nil {
		waking.Annotations = map[string]string{}
	}
	waking.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, env.Client.Update(ctx, &waking), "stamp wake annotation")
	reconcileToWork(t, ctx, r, "s-browser-del")

	var failed spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, key, &failed), "get session after wake")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionBrowserBootFailed, failed.Status.FailureReason,
		"a wake with a deleted identity must fail closed with BrowserBootFailed")
	cond := meta.FindStatusCondition(failed.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
	require.NotNil(t, cond, "a Failed condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "btb-del-id", "the failure must name the missing identity")
}
