// pkg/controllers/agentsession/credential_grants_sidecar_test.go
//
// Unit tests for reconcileCredentialGrants' sidecar-toolbox enumeration: the
// coverage gap this task closes. Before this, a sidecar credential's grant
// was never written, so revoking it had nothing to revoke — the sandbox/MCP
// surfaces get a use_token grant from this same function, but sidecars did
// not. Uses a fake controller-runtime client (no envtest), mirroring
// sidecar_federated_test.go's style.
package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
)

// TestReconcileCredentialGrants_SidecarEnumeration proves the sidecar's
// upstream credential lands in the session's desired externaltoken grant
// set, keyed by the SAME CredentialSource → credID derivation
// materializeSidecarSecret's handout check uses (both call
// resolveSidecarCredential), so a grant written here is found by that check.
func TestReconcileCredentialGrants_SidecarEnumeration(t *testing.T) {
	const (
		ns          = "default"
		sidecarRef  = "linear-tb"
		envVar      = "UPSTREAM_TOKEN"
		credSecret  = "linear-upstream-secret"
		secretValue = "super-secret-upstream-token"
		hashKey     = "session-args-hash-key"
	)

	upstreamSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credSecret, Namespace: ns},
		Data:       map[string][]byte{"token": []byte(secretValue)},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: sidecarRef + "-creds",
				Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: credSecret, Key: "token"},
				},
			}},
		},
	}
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: sidecarRef, Namespace: ns},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "linear-token", EnvVar: envVar},
		},
	}

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(upstreamSecret, ai, tb).Build()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-linear", Namespace: ns},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentIdentity: "ai-linear",
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
				{Name: "linear", Ref: sidecarRef},
			},
		},
	}

	granter := &fakeTokenGranter{}
	r := &Reconciler{Client: c, TokenGranter: granter}

	require.NoError(t, r.reconcileCredentialGrants(context.Background(), sess, ac, []byte(hashKey)))

	// The expected Source + credID mirror exactly what
	// materializeSidecarSecret's handout check derives — both go through
	// resolveSidecarCredential → credresolve.SourceFor.
	wantSrc, err := credresolve.SourceFor(&ai.Spec.Credentials[0], credresolve.RuntimeIdentityFromAgentIdentity(ai))
	require.NoError(t, err)
	wantCredID := externaltoken.CredID(wantSrc)
	wantHash := externaltoken.ValueHash([]byte(hashKey), secretValue)

	require.Len(t, granter.touchedCaveated, 1, "exactly one value-bound grant must be written for the sidecar credential")
	assert.Equal(t, wantCredID+":"+wantHash, granter.touchedCaveated[0])
	assert.Empty(t, granter.touchedIdentity)
}

// TestReconcileCredentialGrants_SidecarMissingToolbox_SkipsNotFatal proves a
// dangling SidecarToolbox ref does not fail the whole grant reconcile: the
// sidecar-materialization loop (which runs immediately afterward in the same
// AgentSession reconcile) performs the authoritative "SidecarToolbox
// missing" boot failure, mirroring how an absent MCPServer/SpiceboxToolspec
// is skipped here rather than duplicated.
func TestReconcileCredentialGrants_SidecarMissingToolbox_SkipsNotFatal(t *testing.T) {
	const ns = "default"

	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-linear", Namespace: ns},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ai).Build()

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s-linear", Namespace: ns},
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentIdentity: "ai-linear",
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{
				{Name: "linear", Ref: "does-not-exist"},
			},
		},
	}

	granter := &fakeTokenGranter{}
	r := &Reconciler{Client: c, TokenGranter: granter}

	err := r.reconcileCredentialGrants(context.Background(), sess, ac, []byte("k"))
	require.NoError(t, err, "a dangling SidecarToolbox ref must not fail the grant reconcile")
	assert.Empty(t, granter.touchedCaveated)
	assert.Empty(t, granter.touchedIdentity)
}

// TestReconcileCredentialGrants_OAuthCredential_IsIdentityOnly proves an oauth
// credential gets an IDENTITY-ONLY grant (like federated), NOT a value-bound
// one. oauth tokens refresh out-of-band (the runner JIT-refreshes on expiry/401),
// so binding the value the operator resolves at reconcile time would false-deny
// after any refresh; worse, an oauth token already EXPIRED at reconcile time
// resolved to no value at all and — under the old value-bound policy — got NO
// grant, denying every subsequent (refreshed) use. Identity-only authorizes the
// credential independent of its ever-changing value. The fixture uses an expired
// oauth Secret to make that regression concrete: the grant must still be written.
func TestReconcileCredentialGrants_OAuthCredential_IsIdentityOnly(t *testing.T) {
	const (
		ns          = "default"
		sidecarRef  = "linear-tb"
		oauthSecret = "linear-oauth-secret"
	)

	// expires_at in the past → the old value-bound path skipped this credential
	// entirely (ErrExpired) and wrote no grant; identity-only writes it regardless.
	upstreamSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: oauthSecret, Namespace: ns},
		Data: map[string][]byte{
			"access_token": []byte("stale-access-token"),
			"expires_at":   []byte("2020-01-01T00:00:00Z"),
		},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: sidecarRef + "-creds",
				Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: oauthSecret},
				},
			}},
		},
	}
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: sidecarRef, Namespace: ns},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "linear-token", EnvVar: "UPSTREAM_TOKEN"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(upstreamSecret, ai, tb).Build()

	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-linear", Namespace: ns}}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentIdentity:    "ai-linear",
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "linear", Ref: sidecarRef}},
		},
	}

	granter := &fakeTokenGranter{}
	r := &Reconciler{Client: c, TokenGranter: granter}

	require.NoError(t, r.reconcileCredentialGrants(context.Background(), sess, ac, []byte("session-args-hash-key")))

	wantSrc, err := credresolve.SourceFor(&ai.Spec.Credentials[0], credresolve.RuntimeIdentityFromAgentIdentity(ai))
	require.NoError(t, err)
	wantCredID := externaltoken.CredID(wantSrc)

	assert.Equal(t, []string{wantCredID}, granter.touchedIdentity,
		"an oauth credential must get an identity-only grant, independent of its refreshing/expired value")
	assert.Empty(t, granter.touchedCaveated,
		"an oauth credential must NOT get a value-bound grant")
}

// TestReconcileCredentialGrants_FederatedCredential_IsIdentityOnly proves a
// federated sidecar credential also gets an identity-only grant: k.Minted()
// drives the same desiredGrant{IdentityOnly: true} branch that
// k.NeedsRefresh() drives for oauth above. Federated is minted fresh on every
// resolve — there is no stable value at reconcile time to pin a caveat to —
// so a value-bound grant would false-deny after every mint. No IdP-identity
// Secret needs to exist for this: reconcileCredentialGrants derives the
// CredentialSource (and therefore the grant) purely from the credential's own
// declared block; the actual mint happens later, in materializeSidecarSecret.
func TestReconcileCredentialGrants_FederatedCredential_IsIdentityOnly(t *testing.T) {
	const (
		ns         = "default"
		sidecarRef = "linear-tb"
	)

	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: sidecarRef + "-creds",
				Type: "federated",
				Federated: &spiceboxv1alpha1.FederatedCredentialSource{
					Resource:          "linear-res",
					ResourceServerURL: "https://mcp.example.invalid",
					IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "u-alice-idp-identity"},
				},
			}},
		},
	}
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: sidecarRef, Namespace: ns},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			UpstreamAuth: spiceboxv1alpha1.SidecarToolboxUpstream{Provider: "linear-token", EnvVar: "UPSTREAM_TOKEN"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(ai, tb).Build()

	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s-linear", Namespace: ns}}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac-linear", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			AgentIdentity:    "ai-linear",
			SidecarToolboxes: []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "linear", Ref: sidecarRef}},
		},
	}

	granter := &fakeTokenGranter{}
	r := &Reconciler{Client: c, TokenGranter: granter}

	require.NoError(t, r.reconcileCredentialGrants(context.Background(), sess, ac, []byte("session-args-hash-key")))

	wantSrc, err := credresolve.SourceFor(&ai.Spec.Credentials[0], credresolve.RuntimeIdentityFromAgentIdentity(ai))
	require.NoError(t, err)
	wantCredID := externaltoken.CredID(wantSrc)

	assert.Equal(t, []string{wantCredID}, granter.touchedIdentity,
		"a federated credential must get an identity-only grant — it is minted fresh per use, so there is no stable value to pin")
	assert.Empty(t, granter.touchedCaveated,
		"a federated credential must NOT get a value-bound grant")
}
