package agentsession

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	// Registers the static/oauth/federated credkind Kinds so this package's
	// unit tests exercise the real registry dispatch: credresolve.SourceFor /
	// ResolveSecretValue now call registry.Get(cred.Type) internally, and
	// nothing else in this package imports a credkind package.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthrough"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func TestRequiredCredentials_MCP(t *testing.T) {
	linear := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "linear",
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "linear-oauth"},
		},
	}
	hubspot := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "hubspot", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "hubspot",
			// Per the no-inference contract, credential names must be
			// declared explicitly — there is no metadata.name fallback.
			Auth: spiceboxv1alpha1.MCPServerAuth{Provider: "hubspot-oauth", Credential: "hubspot"},
		},
	}
	public := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "default"},
		Spec:       spiceboxv1alpha1.MCPServerSpec{Name: "public"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(linear, hubspot, public).Build()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "linear", Ref: "linear"},
				{Name: "hubspot", Ref: "hubspot"},
				{Name: "public", Ref: "public"},
			},
		},
	}
	got, err := RequiredCredentials(context.Background(), c, ac)
	require.NoError(t, err)
	assert.Equal(t, []string{"hubspot", "linear-oauth"}, got)
}

func TestBuildSessionUserIdentity(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
	}
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-x"},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:abc",
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{Name: "linear-oauth", Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "u-x-linear-oauth"},
					}},
				{Name: "github-pat", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "u-x-github-pat", Key: "token"},
					}},
			},
		},
	}

	t.Run("all required credentials covered: empty missing, subset bound", func(t *testing.T) {
		suid, missing := BuildSessionUserIdentity(sess, ui, "user:abc", []string{"linear-oauth"}, nil)
		assert.Empty(t, missing)
		assert.Equal(t, "s1", suid.Name)
		assert.Equal(t, "default", suid.Namespace)
		require.Len(t, suid.OwnerReferences, 1)
		assert.Equal(t, "AgentSession", suid.OwnerReferences[0].Kind)
		require.Len(t, suid.Spec.Credentials, 1)
		assert.Equal(t, "linear-oauth", suid.Spec.Credentials[0].Name)
		assert.Equal(t, "u-x", suid.Spec.UserIdentity)
		assert.Equal(t, "user:abc", suid.Spec.Subject)
	})

	t.Run("uncovered credential: reported missing", func(t *testing.T) {
		_, missing := BuildSessionUserIdentity(sess, ui, "user:abc", []string{"linear-oauth", "hubspot-oauth"}, nil)
		assert.Equal(t, []string{"hubspot-oauth"}, missing)
	})

	t.Run("nil UserIdentity: all required reported missing, empty subset", func(t *testing.T) {
		suid, missing := BuildSessionUserIdentity(sess, nil, "user:abc", []string{"linear-oauth"}, nil)
		assert.Equal(t, []string{"linear-oauth"}, missing)
		assert.Empty(t, suid.Spec.Credentials)
	})
}

func TestBuildSessionUserIdentity_FederatedServer_SynthesizesCredentialNoParking(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns"}}
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-abc"},
		Spec:       spiceboxv1alpha1.UserIdentitySpec{Subject: "user:YWxpY2U="},
	}
	feds := []passthrough.FederatedTarget{{
		CredentialName:    "linear",
		Resource:          "linear-res",
		ResourceServerURL: "https://mcp.example.com",
	}}

	suid, missing := BuildSessionUserIdentity(sess, ui, "user:YWxpY2U=", nil, feds)
	assert.Empty(t, missing, "federated targets must not park")
	require.Len(t, suid.Spec.Credentials, 1)
	c := suid.Spec.Credentials[0]
	assert.Equal(t, "linear", c.Name)
	assert.Equal(t, "federated", c.Type)
	require.NotNil(t, c.Federated)
	assert.Equal(t, "linear-res", c.Federated.Resource)
	assert.Equal(t, "https://mcp.example.com", c.Federated.ResourceServerURL)
	assert.Equal(t, useridentity.IdPIdentitySecretName("user:YWxpY2U="), c.Federated.IdPSecretRef.Name)
}

// TestBuildSessionUserIdentity_FederatedServer_NilUserIdentity_UsesSessionSubject
// pins the rule that a nil ui — a federated-only user with no UserIdentity CR —
// still yields IdPIdentitySecretName(subject), the session's authoritative
// subject, and never IdPIdentitySecretName(""). An empty subject names a Secret
// that can never be found, which the runner experiences as a silent hang.
func TestBuildSessionUserIdentity_FederatedServer_NilUserIdentity_UsesSessionSubject(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns"}}
	feds := []passthrough.FederatedTarget{{
		CredentialName:    "linear",
		Resource:          "https://linear.api.example.invalid",
		ResourceServerURL: "https://mcp.example.com",
	}}

	// nil ui — the common real case for a federated-only user who has no
	// UserIdentity CR. The subject is the session's annotation value.
	suid, missing := BuildSessionUserIdentity(sess, nil /*ui*/, "user:alice", nil, feds)
	assert.Empty(t, missing, "federated targets must never appear in missing")
	require.Len(t, suid.Spec.Credentials, 1)
	c := suid.Spec.Credentials[0]
	assert.Equal(t, "federated", c.Type)
	require.NotNil(t, c.Federated)

	// The IdP-secret name must derive from "user:alice", the session subject,
	// and never from "", which names a non-existent Secret.
	assert.Equal(t, useridentity.IdPIdentitySecretName("user:alice"), c.Federated.IdPSecretRef.Name,
		"synthesized federated cred must reference IdPIdentitySecretName(subject), not IdPIdentitySecretName(\"\")")
	assert.NotEqual(t, useridentity.IdPIdentitySecretName(""), c.Federated.IdPSecretRef.Name,
		"IdPIdentitySecretName(\"\") is the pre-fix bug — must not be referenced")
}

func TestRequiredCredentials_MCPRemap(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name: "linear",
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: "linear-oauth"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(srv).Build()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			MCPServers: []spiceboxv1alpha1.AgentClassMCPServerRef{
				{Name: "linear", Ref: "linear",
					CredentialRemap: map[string]string{"linear-oauth": "linear-admin"}},
			},
		},
	}
	got, err := RequiredCredentials(context.Background(), c, ac)
	require.NoError(t, err)
	assert.Equal(t, []string{"linear-admin"}, got)
}

func TestBuildPassthroughSecretRBAC(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
	}

	t.Run("oauth master Secrets: get-only Role in the identities namespace", func(t *testing.T) {
		role, rb := BuildPassthroughSecretRBAC(sess, []string{"u-x-hubspot-oauth", "u-x-linear-oauth"})
		require.NotNil(t, role)
		assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, role.Namespace)
		assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, rb.Namespace)
		require.Len(t, role.Rules, 1)
		assert.Equal(t, []string{"u-x-hubspot-oauth", "u-x-linear-oauth"}, role.Rules[0].ResourceNames)
		assert.ElementsMatch(t, []string{"get"}, role.Rules[0].Verbs,
			"the runner reads the access token; it cannot refresh (the redemption material "+
				"is in a sibling Secret it is not granted), so it has no reason to write")
		require.Len(t, rb.Subjects, 1)
		assert.Equal(t, "s1-runner-sa", rb.Subjects[0].Name)
		assert.Equal(t, "default", rb.Subjects[0].Namespace)
		assert.Equal(t, role.Name, rb.RoleRef.Name)
		assert.Equal(t, PassthroughRoleName(sess), role.Name)
	})

	t.Run("no oauth masters (all-static): nil, nil — never an empty-ResourceNames all-Secrets grant", func(t *testing.T) {
		role, rb := BuildPassthroughSecretRBAC(sess, nil)
		assert.Nil(t, role)
		assert.Nil(t, rb)
	})
}

// TestPassthroughRunnerGrant_ExposesNoOAuthRedemptionMaterial spans the join
// between how a user's OAuth credential is PERSISTED and what the runner
// ServiceAccount is GRANTED — the two halves that, read separately, each look
// correct.
//
// A Kubernetes `get` on a Secret returns every key in it, so co-locating the
// RFC 6749 redemption material (token_endpoint + client_id + client_secret)
// with the access token in one Secret means any grant that lets the runner read
// the access token also hands it a self-contained, offline-usable refresh grant
// for the user's upstream account: long-lived, usable after session teardown,
// unaffected by broker InvalidateSecret (which drops a cache, not a token), and
// invisible to toolguard, the audit log and SpiceDB. The runner is the
// least-trusted process in the system — every capability-in-context gate exists
// to contain it.
//
// So the invariant is not about any one writer: NOTHING the passthrough grant
// names may carry redemption material, and the grant must not let the runner
// write the user's credential either.
func TestPassthroughRunnerGrant_ExposesNoOAuthRedemptionMaterial(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build()

	// Persist a confidential-client OAuth credential exactly as the identityd
	// OAuth callback does.
	req := useridentity.PutOAuthTokenRequest{
		Subject:        "user:alice@example.invalid",
		CredentialName: "linear-oauth",
		AccessToken:    "at-1",
		RefreshToken:   "rt-1",
		TokenType:      "Bearer",
		TokenEndpoint:  "https://idp.example.invalid/token",
		ClientID:       "dcr-client-id",
		ClientSecret:   "dcr-client-secret",
	}
	require.NoError(t, useridentity.PutOAuthToken(ctx, c, req))

	uiName := useridentity.NameForSubject(req.Subject)
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: req.CredentialName, Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{
						Name: useridentity.MasterSecretName(uiName, req.CredentialName),
					},
				},
			}},
		},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
	}
	role, _ := BuildPassthroughSecretRBAC(sess, oauthMasterSecretNames(ctx, suid))
	require.NotNil(t, role, "a passthrough session with an oauth credential must get a grant")
	require.Len(t, role.Rules, 1)

	assert.Equal(t, []string{"get"}, role.Rules[0].Verbs,
		"read-only: the runner cannot refresh a credential whose redemption material it cannot read, "+
			"so `update` is pure attack surface — it would let a compromised runner clobber the user's token")

	// Every Secret the runner may read must be free of redemption material.
	for _, name := range role.Rules[0].ResourceNames {
		var sec corev1.Secret
		require.NoError(t, c.Get(ctx, client.ObjectKey{
			Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: name,
		}, &sec), "granted Secret %q must exist", name)
		for _, key := range []string{"token_endpoint", "client_id", "client_secret"} {
			assert.NotContainsf(t, sec.Data, key,
				"Secret %q is runner-readable and carries %q: that plus refresh_token is a complete "+
					"offline refresh grant for the user's upstream account", name, key)
		}
	}
}

// TestBuildPassthroughCredentialSecretRBAC pins the per-session credential
// reader grant: get-only on EXACTLY the projected Secret, in the session
// namespace, owned by the AgentSession (owner-ref GC).
func TestBuildPassthroughCredentialSecretRBAC(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
	}

	t.Run("projected Secret present: get-only Role in the session namespace, session-owned", func(t *testing.T) {
		secretName := spiceboxv1alpha1.PassthroughCredentialSecretName(sess.Name)
		role, rb := BuildPassthroughCredentialSecretRBAC(sess, secretName)
		require.NotNil(t, role)
		assert.Equal(t, "default", role.Namespace)
		assert.Equal(t, "default", rb.Namespace)
		require.Len(t, role.Rules, 1)
		assert.Equal(t, []string{secretName}, role.Rules[0].ResourceNames)
		assert.Equal(t, []string{"get"}, role.Rules[0].Verbs, "read-only: the projected copy is never refreshed")
		require.Len(t, role.OwnerReferences, 1)
		assert.Equal(t, "AgentSession", role.OwnerReferences[0].Kind)
		require.Len(t, rb.Subjects, 1)
		assert.Equal(t, "s1-runner-sa", rb.Subjects[0].Name)
		assert.Equal(t, role.Name, rb.RoleRef.Name)
	})

	t.Run("no projected Secret: nil, nil", func(t *testing.T) {
		role, rb := BuildPassthroughCredentialSecretRBAC(sess, "")
		assert.Nil(t, role)
		assert.Nil(t, rb)
	})
}

// TestMaterializePassthroughCredentials verifies the operator projects a
// type=static credential's VALUE from its master Secret (identities namespace)
// into the per-session Secret (session namespace), keyed by credential name and
// owned by the AgentSession.
func TestMaterializePassthroughCredentials(t *testing.T) {
	master := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "u-x-github-pat", Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("ghp_secret")},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).WithObjects(master).Build()
	r := &Reconciler{Client: c}

	staticCreds := []spiceboxv1alpha1.AgentCredential{{
		Name: "github-token", Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "u-x-github-pat", Key: "token"},
		},
	}}
	_, err := r.materializePassthroughCredentials(context.Background(), sess, staticCreds)
	require.NoError(t, err)

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: spiceboxv1alpha1.PassthroughCredentialSecretName("s1")}, &got),
		"per-session credential Secret must be created in the session namespace")
	// The fake client stores StringData as-is (the real apiserver folds it into
	// Data); guard against both.
	val := got.StringData["github-token"]
	if val == "" {
		val = string(got.Data["github-token"])
	}
	assert.Equal(t, "ghp_secret", val, "projected value keyed by credential name")
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", got.OwnerReferences[0].Kind)
	assert.Equal(t, "s1", got.OwnerReferences[0].Name)
}

// TestBuildPassthroughSecretRBAC_VerbsAreEscalation_RequireOperatorMatch
// pins the privilege-escalation contract that bit a real user: the
// per-session passthrough Role grants a verb subset on master Secrets,
// so the operator's own ClusterRole MUST hold those same verbs. K8s
// refuses to let a SA create a Role granting a verb the SA does not
// itself hold, returning the cryptic 'is attempting to grant RBAC
// permissions not currently held' error.
//
// This test fails loudly if someone adds a verb to BuildPassthrough
// SecretRBAC.Verbs without also adding it to the operator's
// +kubebuilder:rbac:groups="",resources=secrets marker in
// agentsession/controller.go. The check is verb-list equality: any
// new verb introduced here forces the maintainer to update the
// marker (and that file's comment) in the same PR.
func TestBuildPassthroughSecretRBAC_VerbsAreEscalation_RequireOperatorMatch(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
	}
	role, _ := BuildPassthroughSecretRBAC(sess, []string{"u-x-secret"})
	require.Len(t, role.Rules, 1)

	// Operator's ClusterRole-bestowed verbs on secrets (from the
	// agentsession/controller.go +kubebuilder:rbac marker). Update
	// this list IN LOCK STEP with the marker.
	operatorSecretVerbs := map[string]bool{
		"get":    true,
		"list":   true,
		"watch":  true,
		"create": true,
		"update": true,
		"patch":  true,
	}
	for _, v := range role.Rules[0].Verbs {
		assert.Truef(t, operatorSecretVerbs[v],
			"verb %q granted to runner via passthrough Role but NOT in the operator's ClusterRole — "+
				"K8s will return 'is attempting to grant RBAC permissions not currently held' "+
				"on Role apply. Add %q to the +kubebuilder:rbac marker in agentsession/controller.go (groups=\"\",resources=secrets) so the operator holds it before granting it.",
			v, v)
	}
}

// TestOauthMasterSecretNames_And_StaticPassthroughCredentials_ClassifyByKind
// locks in the credkind-registry wiring both collectors dispatch through:
// oauthMasterSecretNames must select exactly the NeedsRefresh credential
// (oauth) and staticPassthroughCredentials must select exactly the
// Projectable one (static) — federated is excluded from both (it is neither
// stored-and-refreshed nor value-projectable; it is minted on demand). This
// does not distinguish the pre-migration literal-string dispatch from the
// post-migration registry dispatch — for today's three registered kinds the
// two forms agree on every input by construction — but it guards the WIRING
// against a future mix-up (e.g. NeedsRefresh swapped for Minted, or
// Projectable swapped for !Minted) that a decorative "no error" assertion
// would not catch.
func TestOauthMasterSecretNames_And_StaticPassthroughCredentials_ClassifyByKind(t *testing.T) {
	ctx := context.Background()
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "static-cred", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "static-secret", Key: "token"},
					},
				},
				{
					Name: "oauth-cred", Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "oauth-master-secret"},
					},
				},
				{
					Name: "fed-cred", Type: "federated",
					Federated: &spiceboxv1alpha1.FederatedCredentialSource{
						IdPSecretRef: spiceboxv1alpha1.SecretRef{Name: "idp-secret"},
					},
				},
			},
		},
	}

	masters := oauthMasterSecretNames(ctx, suid)
	assert.Equal(t, []string{"oauth-master-secret"}, masters,
		"only the oauth (NeedsRefresh) credential's master Secret is read cross-namespace by the runner")

	projected := staticPassthroughCredentials(ctx, suid)
	require.Len(t, projected, 1)
	assert.Equal(t, "static-cred", projected[0].Name,
		"only the static (Projectable) credential is projected into the per-session Secret")
}

// TestOauthMasterSecretNames_And_StaticPassthroughCredentials_SkipUnregisteredType
// pins the skip-and-log branch in both collectors: credkindregistry.Get
// erroring for an unregistered type must skip that credential (log-only, per
// AGENTS.md's no-silent-errors rule) rather than panic or leak a zero-value
// Secret name into the runner's scoped RBAC allowlist or the static
// projection set. Only the error-RETURNING third site
// (BuildSessionUserIdentity's federated-classify loop, passthrough.go:427)
// had coverage for an unregistered type before this; these two silent-skip
// collectors did not.
func TestOauthMasterSecretNames_And_StaticPassthroughCredentials_SkipUnregisteredType(t *testing.T) {
	ctx := context.Background()
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{Name: "bogus-cred", Type: "bogus-unregistered-type"},
				{
					Name: "oauth-cred", Type: "oauth",
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretRef{Name: "oauth-master-secret"},
					},
				},
			},
		},
	}

	masters := oauthMasterSecretNames(ctx, suid)
	assert.Equal(t, []string{"oauth-master-secret"}, masters,
		"an unregistered-type credential must be skipped, never crash the collector or leak a zero-value Secret name")

	projected := staticPassthroughCredentials(ctx, suid)
	assert.Empty(t, projected, "an unregistered-type credential must be skipped from static projection too")
}
