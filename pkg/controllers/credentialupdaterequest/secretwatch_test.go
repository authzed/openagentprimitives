// pkg/controllers/credentialupdaterequest/secretwatch_test.go
//
// Covers mapSecretToRequests -- the enqueue half of unparking. reconcileOpen's
// own behaviour (a changed Secret marks the request Fulfilled) is unpark_test.go;
// this file asks the prior question: would a Secret write in production ever
// have woken the request at all?
//
// The fixtures here deliberately break the package's coverage monoculture. Every
// other reconcile-level fixture resolves a type=static passthrough credential,
// whose value the operator projects into a per-session Secret in the SESSION's
// namespace -- so "the CR's namespace" and "the Secret's namespace" are the same
// string in every one of them, and a selector that conflates the two is
// indistinguishable from a correct one. A user-owned type=oauth credential is
// the opposite shape: credresolve.SourceFor anchors it in the platform's shared
// identities namespace (that is where JIT refresh lives), while the request
// itself lives in the session's. No CredentialUpdateRequest ever exists in the
// identities namespace.
package credentialupdaterequest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// oauthMasterSecretName is the Secret backing the user-owned OAuth credential
// in this file's fixtures. It lives in IdentitiesNamespace, never in ns.
const oauthMasterSecretName = "demo-oauth-master"

// deadRefreshOAuthWorld builds a userPassthrough session whose single
// credential is type=oauth, plus its master Secret in the platform identities
// namespace and a token endpoint that answers invalid_grant.
//
// invalid_grant is not incidental: it is the one refresh failure Determine
// treats as definitive (RejectedRefreshDead, the design's headline OAuth
// verdict), so it is what actually drives an OAuth request to Open and makes
// the unpark watch matter for this shape at all.
func deadRefreshOAuthWorld(t *testing.T) []client.Object {
	t.Helper()
	tokenEndpoint := installDeadRefreshTokenEndpoint(t)

	cred := oauthCredential()
	cred.OAuth.SecretRef = spiceboxv1alpha1.SecretRef{Name: oauthMasterSecretName}
	sess, class, suid, mcp, cur := baseObjects(cred, "")
	return []client.Object{sess, class, suid, mcp, cur,
		deadOAuthMasterSecret(oauthMasterSecretName, tokenEndpoint)}
}

// installDeadRefreshTokenEndpoint stands up an OAuth token endpoint that
// answers invalid_grant and routes refresh.Run through it for the test's
// lifetime, returning its URL for the master Secret's token_endpoint key.
//
// invalid_grant is not incidental: it is the one refresh failure Determine
// treats as definitive (RejectedRefreshDead, the design's headline OAuth
// verdict), so it is what actually drives a user-owned OAuth request to Open.
//
// refresh.SetHTTPClient is package-level, global, mutable state -- tests using
// this must NOT run t.Parallel(), the same hazard installRedirectClient carries.
func installDeadRefreshTokenEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token is dead"}`))
	}))
	t.Cleanup(srv.Close)
	refresh.SetHTTPClient(srv.Client())
	t.Cleanup(func() { refresh.SetHTTPClient(safehttp.Client()) })
	return srv.URL
}

// deadOAuthMasterSecret is a user-owned OAuth credential's master Secret, in
// the platform identities namespace where credresolve.SourceFor anchors every
// one of them -- never in the session's own namespace.
func deadOAuthMasterSecret(name, tokenEndpoint string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data: map[string][]byte{
			"access_token":   []byte("dead-access-token"),
			"refresh_token":  []byte("dead-refresh-token"),
			"token_endpoint": []byte(tokenEndpoint),
			"client_id":      []byte("client-abc"),
		},
	}
}

// enqueuedNames renders a map func's result as namespace/name strings, so a
// failure message names the requests that WERE selected rather than printing a
// struct dump.
func enqueuedNames(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, rq := range reqs {
		out = append(out, rq.Namespace+"/"+rq.Name)
	}
	return out
}

// TestMapSecretToRequests_CrossNamespaceSecretStillEnqueuesTheRequest is the
// regression guard for the failure this file exists to make representable.
//
// A user-owned OAuth credential's Secret lives in the platform identities
// namespace; the request lives in the session's. Selecting candidate requests by
// the SECRET's namespace therefore searches a namespace no CredentialUpdateRequest
// is ever created in, and the watch selects nothing at all. Nothing about that is
// visible from the outside: the request stays Open, and the only remaining wakeup
// is its own idle-TTL requeue -- which fires long after the meta tool has already
// given up and told the agent nobody answered. A human pastes the replacement,
// and the platform reports that nobody did.
func TestMapSecretToRequests_CrossNamespaceSecretStillEnqueuesTheRequest(t *testing.T) {
	ctx := context.Background()
	c, r, _ := newReconciler(t, deadRefreshOAuthWorld(t)...)

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "Reconcile")

	opened := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, opened.Status.Phase,
		"control: the fixture must really open a card, or the enqueue below has nothing to select; status=%+v",
		opened.Status)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedRefreshDead, opened.Status.Determination)
	require.NotNil(t, opened.Status.CredentialSecretRef, "the Open transition must record the backing Secret")

	// The discriminating fact, asserted rather than assumed: the Secret this
	// request is watching is in a DIFFERENT namespace from the request itself.
	require.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, opened.Status.CredentialSecretRef.Namespace,
		"control: a user-owned OAuth credential's master Secret lives in the identities namespace")
	require.NotEqual(t, opened.Namespace, opened.Status.CredentialSecretRef.Namespace,
		"control: the request and its backing Secret must really be in different namespaces, or this proves nothing")

	var master corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: opened.Status.CredentialSecretRef.Namespace,
		Name:      opened.Status.CredentialSecretRef.Name,
	}, &master))

	got := r.MapSecretToRequestsForTest(ctx, &master)
	assert.Equal(t, []string{ns + "/" + curName}, enqueuedNames(got),
		"a human replacing this credential writes the master Secret in the identities namespace; the request "+
			"waiting on it lives in the session namespace, and it must still be woken")
}

// TestMapSecretToRequests_Selection sweeps everything the selector must still
// REFUSE to enqueue once it no longer restricts the search to the Secret's own
// namespace. Dropping a namespace filter is only correct if the per-item
// comparison keeps both halves of the reference; the "same name, wrong
// namespace" row is what makes that a fact rather than a hope.
//
// Every row shares one seeded world: three requests in the session namespace,
// differing only in the status the selector reads.
func TestMapSecretToRequests_Selection(t *testing.T) {
	const (
		watchedSecretNS   = spiceboxv1alpha1.IdentitiesNamespace
		watchedSecretName = "demo-watched-secret"
		sameNSSecretName  = "demo-projected-secret"
		openMatching      = "demo-cur-open-matching"
		openOther         = "demo-cur-open-other-secret"
		openSameNS        = "demo-cur-open-same-namespace"
		refusedMatching   = "demo-cur-refused-matching"
	)

	seed := func(name, phase string, ref *spiceboxv1alpha1.NamespacedRef) *spiceboxv1alpha1.CredentialUpdateRequest {
		cur := requestFor(name, sessionAName, sessionAUID, mcpName)
		cur.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
			Phase:               phase,
			Determination:       spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			Reason:              "seeded",
			CredentialSecretRef: ref,
		}
		return cur
	}

	world := []client.Object{
		seed(openMatching, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			&spiceboxv1alpha1.NamespacedRef{Namespace: watchedSecretNS, Name: watchedSecretName}),
		seed(openOther, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			&spiceboxv1alpha1.NamespacedRef{Namespace: watchedSecretNS, Name: "demo-unrelated-secret"}),
		seed(refusedMatching, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			&spiceboxv1alpha1.NamespacedRef{Namespace: watchedSecretNS, Name: watchedSecretName}),
		seed(openSameNS, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			&spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sameNSSecretName}),
	}

	cases := []struct {
		name   string
		secret *corev1.Secret
		want   []string
	}{
		{
			// The one shape a namespace-restricted search could ever find, and so
			// the control that proves the selector works at all: a type=static
			// passthrough credential's projected Secret sits in the session's own
			// namespace alongside the request.
			name: "a same-namespace Secret: the Open request that names it is enqueued (control -- the finder works)",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: sameNSSecretName}},
			want: []string{ns + "/" + openSameNS},
		},
		{
			name: "the watched cross-namespace Secret: only the Open request that names it is enqueued",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: watchedSecretNS, Name: watchedSecretName}},
			want: []string{ns + "/" + openMatching},
		},
		{
			name: "same NAME in a different namespace: enqueues nothing, so the ref comparison still carries both halves",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: "demo-other-identities", Name: watchedSecretName}},
			want: []string{},
		},
		{
			name: "a Secret nothing records: enqueues nothing",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: watchedSecretNS, Name: "demo-nobody-watches-this"}},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r, _ := newReconciler(t, world...)
			assert.Equal(t, tc.want, enqueuedNames(r.MapSecretToRequestsForTest(context.Background(), tc.secret)),
				"a Refused request naming the same Secret must never be enqueued either -- it has no card to unpark")
		})
	}
}
