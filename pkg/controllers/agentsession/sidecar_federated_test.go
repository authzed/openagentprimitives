// pkg/controllers/agentsession/sidecar_federated_test.go
//
// Unit test for materializeSidecarSecret when the upstream credential is
// type=federated.  Uses a controller-runtime fake client (bypasses CRD
// XValidation, which blocks type=federated on real AgentIdentity objects) so
// the test can construct the fixture without an envtest apiserver.
package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
	federationfake "github.com/authzed/openagentprimitives/pkg/platform/identity/federation/fake"
)

// TestMaterializeSidecarSecret_Federated_MintsToken proves that when a
// sidecar's AgentIdentity carries a type=federated credential, the reconciler:
//  1. calls r.Minter.Mint with the resource/URL/scopes from the federated block,
//  2. writes the minted token into the per-session sidecar Secret under envVar,
//  3. does NOT error.
//
// Uses a fake client so the CRD XValidation rule that blocks type=federated on
// AgentIdentity is not enforced — this is intentional; the validation exists for
// the real API server and is separately enforced there. The unit under test is
// the controller's minting path, not API-server admission.
func TestMaterializeSidecarSecret_Federated_MintsToken(t *testing.T) {
	const (
		ns           = "default"
		idpSecretRef = "u-alice-idp-identity"
		resource     = "linear-res"
		resourceURL  = "https://mcp.example.com"
		envVar       = "UPSTREAM_TOKEN"
		sidecarRef   = "linear"
		credName     = sidecarRef + "-creds" // = <ref>-creds, the convention
		scSecret     = "sc-secret"
	)

	// IdP-identity Secret: holds the user's oauth tokens + IdP endpoints.
	// Placed in the AgentIdentity's namespace (ident.Namespace = ns) because
	// materializeSidecarSecret passes ident.Namespace to SubjectMaterial.
	// No expires_at → treated as non-expired by credresolve.
	idpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: idpSecretRef, Namespace: ns},
		Data: map[string][]byte{
			"access_token":   []byte("user-id-token"),
			"refresh_token":  []byte("rt"),
			"token_endpoint": []byte("https://idp.example.com/token"),
			"client_id":      []byte("ap-client"),
			"client_secret":  []byte("ap-secret"),
		},
	}

	// AgentIdentity carrying a type=federated credential for this sidecar.
	// The fake client does not enforce the XValidation rule that blocks
	// type=federated on AgentIdentity in the real API server.
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-federated", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName,
				Type: "federated",
				Federated: &spiceboxv1alpha1.FederatedCredentialSource{
					Resource:          resource,
					ResourceServerURL: resourceURL,
					IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: idpSecretRef},
					Scopes:            []string{"read"},
				},
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(idpSecret, ai).
		Build()

	m := &federationfake.Minter{TTL: time.Hour}
	r := &Reconciler{Client: c, Minter: m}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-fed", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			AgentIdentity: "ai-federated",
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-fed", Namespace: ns},
	}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: sidecarRef,
		Ref:  sidecarRef,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{
				Provider: "linear-oauth",
				EnvVar:   envVar,
			},
		},
	}

	err := r.materializeSidecarSecret(t.Context(), sess, ac, rt, scSecret, nil)
	require.NoError(t, err, "materializeSidecarSecret must succeed for federated cred")

	// The per-session Secret must exist and carry the minted token.
	// The fake client stores StringData as-is (the real apiserver folds
	// StringData into Data; tests using fake clients must check StringData).
	var sec corev1.Secret
	require.NoError(t,
		c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: scSecret}, &sec),
		"per-session sidecar Secret must be created")
	// fake.Minter returns "minted-for-<resource>".
	got := sec.StringData[envVar]
	if got == "" {
		// real-apiserver path folds StringData → Data; guard against both.
		got = string(sec.Data[envVar])
	}
	assert.Equal(t, "minted-for-"+resource, got,
		"minted token must be projected under the declared env var")

	// The Minter was called with the correct request parameters.
	require.Len(t, m.Calls, 1, "Minter.Mint must be called exactly once")
	call := m.Calls[0]
	assert.Equal(t, resource, call.Resource)
	assert.Equal(t, resourceURL, call.ResourceServerURL)
	assert.Equal(t, []string{"read"}, call.Scopes)
}

// TestMaterializeSidecarSecret_Federated_UseTokenCheckPasses proves the
// one-time pre-handout use_token check does not block a federated (identity-
// only) sidecar credential even though its presented value hash is different
// on every mint. Federated grants carry no authorized_value_hash caveat (see
// credential_grants.go's desiredGrant doc + applyGrantDiff's Federated case:
// TouchAuthorizedTokenIdentity, not TouchAuthorizedToken) — a real SpiceDB
// check permission on that grant ignores the presented context entirely, so
// any presented hash is allowed. A fake TokenChecker configured to allow
// stands in for that here; the test's real assertion is that
// materializeSidecarSecret DOES invoke the check for a federated credential
// (fully-consistent, keyed by the same CredID reconcileCredentialGrants would
// have granted) rather than skipping it because there is no stable value.
func TestMaterializeSidecarSecret_Federated_UseTokenCheckPasses(t *testing.T) {
	const (
		ns           = "default"
		idpSecretRef = "u-alice-idp-identity"
		resource     = "linear-res"
		resourceURL  = "https://mcp.example.com"
		envVar       = "UPSTREAM_TOKEN"
		sidecarRef   = "linear"
		credName     = sidecarRef + "-creds"
		scSecret     = "sc-secret-checked"
		hashKey      = "session-args-hash-key"
	)

	idpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: idpSecretRef, Namespace: ns},
		Data: map[string][]byte{
			"access_token":   []byte("user-id-token"),
			"refresh_token":  []byte("rt"),
			"token_endpoint": []byte("https://idp.example.com/token"),
			"client_id":      []byte("ap-client"),
			"client_secret":  []byte("ap-secret"),
		},
	}

	cred := spiceboxv1alpha1.AgentCredential{
		Name: credName,
		Type: "federated",
		Federated: &spiceboxv1alpha1.FederatedCredentialSource{
			Resource:          resource,
			ResourceServerURL: resourceURL,
			IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: idpSecretRef},
			Scopes:            []string{"read"},
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-federated-checked", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentIdentitySpec{Credentials: []spiceboxv1alpha1.AgentCredential{cred}},
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(idpSecret, ai).
		Build()

	m := &federationfake.Minter{TTL: time.Hour}
	checker := &fakeUseTokenChecker{allowed: true}
	r := &Reconciler{Client: c, Minter: m, TokenChecker: checker}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-fed-checked", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{AgentIdentity: "ai-federated-checked"},
	}
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac-fed-checked", Namespace: ns}}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: sidecarRef,
		Ref:  sidecarRef,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "linear-oauth", EnvVar: envVar},
		},
	}

	err := r.materializeSidecarSecret(t.Context(), sess, ac, rt, scSecret, []byte(hashKey))
	require.NoError(t, err, "use_token check must not block a federated credential")

	var sec corev1.Secret
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: scSecret}, &sec))
	mintedValue := "minted-for-" + resource // fake.Minter's deterministic output
	got := sec.StringData[envVar]
	if got == "" {
		got = string(sec.Data[envVar])
	}
	assert.Equal(t, mintedValue, got)

	// The check ran, keyed by the SAME CredentialSource
	// reconcileCredentialGrants would have derived (credresolve.SourceFor over
	// the identical AgentCredential + RuntimeIdentity), and presented the hash
	// of the just-minted value — proving the federated path is checked, not
	// skipped, even though the presented hash changes on every mint.
	require.Len(t, checker.calls, 1)
	call := checker.calls[0]
	wantSrc, err := credresolve.SourceFor(&cred, credresolve.RuntimeIdentityFromAgentIdentity(ai))
	require.NoError(t, err)
	wantCredID := externaltoken.CredID(wantSrc)
	assert.Equal(t, wantCredID, call.credID)
	assert.Equal(t, externaltoken.ValueHash([]byte(hashKey), mintedValue), call.presented)
	assert.True(t, call.fullyConsistent, "the pre-handout check must be fully-consistent")
}

// TestMaterializeSidecarSecret_Federated_NilMinter_FailsClosed proves that
// when Minter is nil and the sidecar credential is type=federated, the
// reconciler returns a clear error rather than panicking or silently continuing.
func TestMaterializeSidecarSecret_Federated_NilMinter_FailsClosed(t *testing.T) {
	const (
		ns       = "default"
		envVar   = "UPSTREAM_TOKEN"
		credName = "linear-creds"
	)

	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-federated-nominter", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName,
				Type: "federated",
				Federated: &spiceboxv1alpha1.FederatedCredentialSource{
					Resource:          "linear-res",
					ResourceServerURL: "https://mcp.example.com",
					IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "u-alice-idp-identity"},
				},
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(ai).
		Build()

	r := &Reconciler{Client: c, Minter: nil} // no minter wired

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-nilminter", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			AgentIdentity: "ai-federated-nominter",
		},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-nilminter", Namespace: ns},
	}
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "linear",
		Ref:  "linear",
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{
				Provider: "linear-oauth",
				EnvVar:   envVar,
			},
		},
	}

	err := r.materializeSidecarSecret(t.Context(), sess, ac, rt, "sc-secret-nilminter", nil)
	require.Error(t, err, "nil Minter must return an error (fail closed)")
	assert.Contains(t, err.Error(), "no federation minter configured",
		"error must mention that no minter is configured")
}
