package secretoutsrv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
)

// newSession returns an AgentSession object with a stable UID so the
// owner-ref the handler writes can be asserted.
func newSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			UID:       types.UID(ns + "-" + name + "-uid"),
		},
	}
}

// newTestServer builds an httptest.Server over a fresh fake client seeded
// with the given objects plus a token registry.
func newTestServer(t *testing.T, objs ...client.Object) (*httptest.Server, client.Client, *tokens.Registry) {
	t.Helper()
	sch := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	reg := tokens.NewRegistry()
	srv := httptest.NewServer(secretoutsrv.NewHandler(c, reg))
	t.Cleanup(srv.Close)
	return srv, c, reg
}

func postSecretOutput(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err, "marshal body")
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	require.NoError(t, err, "NewRequest")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Do")
	return resp
}

func TestPostSecretOutput_AuthedWrite_204AndSecretKey(t *testing.T) {
	sess := newSession("ns", "sess1")
	srv, c, reg := newTestServer(t, sess)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess1"}, "tok-own", "")

	resp := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name":   "kubeconfig",
		"value":  "super-secret-bytes",
		"handle": "so-abc123",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "authed write → 204")

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-secret-outputs"}, &sec),
		"per-session secret-output Secret exists")
	assert.Equal(t, "super-secret-bytes", string(sec.Data["kubeconfig"]), "Data[name]==value")
	assert.Equal(t, corev1.SecretTypeOpaque, sec.Type, "Opaque type")

	// Owner-ref points back at the AgentSession (Controller + BlockOwnerDeletion).
	require.Len(t, sec.OwnerReferences, 1, "one owner-ref")
	or := sec.OwnerReferences[0]
	assert.Equal(t, "AgentSession", or.Kind, "owner kind")
	assert.Equal(t, "sess1", or.Name, "owner name")
	assert.Equal(t, sess.UID, or.UID, "owner UID")
	require.NotNil(t, or.Controller)
	assert.True(t, *or.Controller, "Controller=true")
	require.NotNil(t, or.BlockOwnerDeletion)
	assert.True(t, *or.BlockOwnerDeletion, "BlockOwnerDeletion=true")
}

func TestPostSecretOutput_NoOrInvalidToken_401(t *testing.T) {
	srv, _, reg := newTestServer(t, newSession("ns", "sess1"))
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess1"}, "tok-own", "")

	cases := []struct {
		name  string
		token string
	}{
		{name: "no token: 401", token: ""},
		{name: "unknown token: 401", token: "bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", tc.token, map[string]string{
				"name": "kubeconfig", "value": "v", "handle": "so-x",
			})
			defer resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "unauthorized")
		})
	}
}

func TestPostSecretOutput_TokenForDifferentSession_403(t *testing.T) {
	srv, c, reg := newTestServer(t, newSession("ns", "sess1"), newSession("ns", "other"))
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "other"}, "tok-other", "")

	resp := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-other", map[string]string{
		"name": "kubeconfig", "value": "v", "handle": "so-x",
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "cross-session token → 403")

	// No Secret should have been written for sess1.
	var sec corev1.Secret
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-secret-outputs"}, &sec)
	assert.True(t, err != nil, "no secret written on 403")
}

// TestPostSecretOutput_ExtraReadScopeCannotWrite_403 pins that an EXTRA scope on a
// per-session token authorizes nothing here.
//
// The AgentSession reconciler registers a runner's token with extra scopes — the
// per-bundle SpiceboxSessions whose ToolCall stdout/stderr artifacts the runner must
// fetch (controllers/agentsession/controller.go, "Authorize artifact reads under
// both …"). tokens.Set states it as a contract: an extra scope is a foreign
// session's data the token may LOOK AT, never one it may write into. This endpoint
// is write-only — every method but POST is 405 — so answering it with the registry's
// READ predicate (Authorizes) let a runner mint a permanent, write-once
// secret-output on any bundle scope it was registered against.
//
// The extras are SpiceboxSession names (<session>-<bundle>) while writeSecretOutput
// Gets an AgentSession, so an arbitrary bundle scope 500s on the owner-ref lookup
// rather than writing. That is a type mismatch three layers below the authorization
// decision, not the decision — and AgentSession names are user-derived, so the
// collision seeded here is representable. The gate must refuse on its own terms.
func TestPostSecretOutput_ExtraReadScopeCannotWrite_403(t *testing.T) {
	// "ns/sess1-bundle" is both an extra READ scope of tok-own and a real
	// AgentSession, so nothing downstream can stand in for the gate.
	srv, c, reg := newTestServer(t, newSession("ns", "sess1"), newSession("ns", "sess1-bundle"))
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess1"}, "tok-own", "",
		memory.NamespacedName{Namespace: "ns", Name: "sess1-bundle"})

	resp := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1-bundle", "tok-own", map[string]string{
		"name": "kubeconfig", "value": "v", "handle": "so-x",
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a read-only extra scope must not authorize a secret-output write")

	var sec corev1.Secret
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-bundle-secret-outputs"}, &sec)
	assert.Error(t, err, "no Secret may be written for a scope the token only reads")

	// The token's OWN session is unaffected — the fix removes the extra-scope
	// disjunct, not the primary-session grant.
	own := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "kubeconfig", "value": "v", "handle": "so-y",
	})
	defer own.Body.Close()
	assert.Equal(t, http.StatusNoContent, own.StatusCode, "the primary session still writes")
}

func TestPostSecretOutput_SystemTokensRejected_403(t *testing.T) {
	srv, _, reg := newTestServer(t, newSession("ns", "sess1"))
	reg.SetChannelsdToken("sys-channelsd")
	reg.SetAuthzdToken("sys-authzd")

	for _, tok := range []string{"sys-channelsd", "sys-authzd"} {
		resp := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", tok, map[string]string{
			"name": "kubeconfig", "value": "v", "handle": "so-x",
		})
		// System tokens are not per-session bearers, so LookupInfo misses → 401.
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, resp.StatusCode,
			"system token rejected (per-session only)")
		resp.Body.Close()
	}
}

func TestPostSecretOutput_TwoNames_Merge(t *testing.T) {
	srv, c, reg := newTestServer(t, newSession("ns", "sess1"))
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess1"}, "tok-own", "")

	r1 := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "kubeconfig", "value": "first", "handle": "so-1",
	})
	require.Equal(t, http.StatusNoContent, r1.StatusCode, "first write 204")
	r1.Body.Close()

	r2 := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "token", "value": "second", "handle": "so-2",
	})
	require.Equal(t, http.StatusNoContent, r2.StatusCode, "second write 204")
	r2.Body.Close()

	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-secret-outputs"}, &sec), "get merged secret")
	assert.Equal(t, "first", string(sec.Data["kubeconfig"]), "first key preserved")
	assert.Equal(t, "second", string(sec.Data["token"]), "second key added (merge)")
}

// TestPostSecretOutput_SecondWriteSameNameRejected covers Task 6's layer-2
// write-once enforcement: a re-publish of a name already written for this
// session is rejected with 409 (defense in depth — the runner's fast-fail
// pre-check is supposed to prevent legitimate retries from ever reaching
// here). A different name on the same session is unaffected.
func TestPostSecretOutput_SecondWriteSameNameRejected(t *testing.T) {
	sess := newSession("ns", "sess1")
	srv, c, reg := newTestServer(t, sess)
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess1"}, "tok-own", "")

	r1 := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "kubeconfig", "value": "first-value", "handle": "so-1",
	})
	require.Equal(t, http.StatusNoContent, r1.StatusCode, "first write 204")
	r1.Body.Close()

	// Second write, same name, any value → 409, body mentions write-once.
	r2 := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "kubeconfig", "value": "second-value", "handle": "so-2",
	})
	defer r2.Body.Close()
	assert.Equal(t, http.StatusConflict, r2.StatusCode, "second write to the same name → 409")
	b2, err := io.ReadAll(r2.Body)
	require.NoError(t, err, "read 409 body")
	assert.Contains(t, string(b2), "write-once", "409 body mentions write-once")
	assert.Contains(t, string(b2), "kubeconfig", "409 body names the secret-output")

	// Secret value unchanged.
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-secret-outputs"}, &sec), "get secret")
	assert.Equal(t, "first-value", string(sec.Data["kubeconfig"]), "value must be unchanged after 409")

	// A byte-identical re-put (same value) is IDEMPOTENT → 204: it restores
	// in-session recoverability when a publish response was lost (the Secret
	// landed, the client saw an error, the retry now succeeds). It does NOT
	// weaken write-once-for-value: only a byte-identical value is accepted.
	r3 := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "kubeconfig", "value": "first-value", "handle": "so-3",
	})
	defer r3.Body.Close()
	assert.Equal(t, http.StatusNoContent, r3.StatusCode, "byte-identical re-put is idempotent → 204")

	// The value is still the original (idempotent re-put changed nothing).
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-secret-outputs"}, &sec), "get secret after idempotent re-put")
	assert.Equal(t, "first-value", string(sec.Data["kubeconfig"]), "value unchanged after idempotent re-put")

	// A different name on the same session still succeeds (204).
	r4 := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", map[string]string{
		"name": "token", "value": "unrelated-value", "handle": "so-4",
	})
	defer r4.Body.Close()
	assert.Equal(t, http.StatusNoContent, r4.StatusCode, "different name on same session → 204")

	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: "ns", Name: "sess1-secret-outputs"}, &sec), "get secret after 4th write")
	assert.Equal(t, "unrelated-value", string(sec.Data["token"]), "different name's value landed")
	assert.Equal(t, "first-value", string(sec.Data["kubeconfig"]), "kubeconfig value still unchanged")
}

func TestPostSecretOutput_BadInput_400(t *testing.T) {
	srv, _, reg := newTestServer(t, newSession("ns", "sess1"))
	reg.Set(memory.NamespacedName{Namespace: "ns", Name: "sess1"}, "tok-own", "")

	cases := []struct {
		name string
		body map[string]string
	}{
		{name: "missing name: 400", body: map[string]string{"value": "v", "handle": "so-x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postSecretOutput(t, srv.URL+"/secret-output/ns/sess1", "tok-own", tc.body)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "bad input")
		})
	}
}
