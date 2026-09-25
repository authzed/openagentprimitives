package setup_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	require.NoError(t, corev1.AddToScheme(s), "corev1")
	return s
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).
		WithObjects(objs...).
		Build()
}

// TestStoreBearerScaffolds verifies that Store creates an AgentIdentity,
// Secret, named credential, and bumps LastSetupAt from nothing.
func TestStoreBearerScaffolds(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "gh-pat",
			BindingEnv:    map[string]string{"GITHUB_TOKEN": ""},
		},
		Value: builtins.StoreValue{Bearer: "ghp_xxxx"},
	}
	require.NoError(t, setup.Store(ctx, c, req))

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.Len(t, ai.Spec.Credentials, 1)
	assert.Equal(t, "gh-pat", ai.Spec.Credentials[0].Name)
	assert.Equal(t, "static", ai.Spec.Credentials[0].Type)

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].Static.SecretRef.Name}, &sec))
	assert.Equal(t, "ghp_xxxx", string(sec.Data[ai.Spec.Credentials[0].Static.SecretRef.Key]))
	assert.NotNil(t, ai.Status.LastSetupAt, "LastSetupAt not set")
}

// TestStoreOAuthScaffolds verifies that Store creates everything from nothing
// for an oauth-shaped credential.
func TestStoreOAuthScaffolds(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "linear-oauth",
			IsBearer:      true,
		},
		Value: builtins.StoreValue{
			OAuth: &builtins.OAuthValue{
				AccessToken:   "at-1",
				RefreshToken:  "rt-1",
				ExpiresIn:     3600,
				TokenEndpoint: "https://auth.example/token",
				ClientID:      "cid",
				ClientSecret:  "csec",
			},
		},
	}
	require.NoError(t, setup.Store(ctx, c, req))
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.Len(t, ai.Spec.Credentials, 1)
	assert.Equal(t, "linear-oauth", ai.Spec.Credentials[0].Name)
	assert.Equal(t, "oauth", ai.Spec.Credentials[0].Type)
	require.NotNil(t, ai.Spec.Credentials[0].OAuth, "credential not oauth")
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].OAuth.SecretRef.Name}, &sec))
	assert.Equal(t, "at-1", string(sec.Data["access_token"]))
	assert.Equal(t, "rt-1", string(sec.Data["refresh_token"]))
	assert.Equal(t, "https://auth.example/token", string(sec.Data["token_endpoint"]))
	assert.Equal(t, "cid", string(sec.Data["client_id"]))
	assert.Equal(t, "csec", string(sec.Data["client_secret"]))
}

// TestStoreIdempotent verifies that re-running Store on a pre-existing
// AgentIdentity + Secret updates the Secret value without duplicating
// credentials.
func TestStoreIdempotent(t *testing.T) {
	// Pre-existing AgentIdentity + Secret with a named credential.
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "gh-pat", Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "my-bot-gh-pat", Key: "token"},
				},
			}},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-bot-gh-pat", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("old")},
	}
	c := newClient(t, ai, sec)
	ctx := context.Background()

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "gh-pat",
			BindingEnv:    map[string]string{"GITHUB_TOKEN": ""},
		},
		Value: builtins.StoreValue{Bearer: "new"},
	}
	require.NoError(t, setup.Store(ctx, c, req))
	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &got))
	assert.Len(t, got.Spec.Credentials, 1, "scaffolding duplicated credentials")
	var gotSec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot-gh-pat"}, &gotSec))
	assert.Equal(t, "new", string(gotSec.Data["token"]), "secret value not updated")
}

// buildIdentityWithStaticCred returns an AgentIdentity that pre-declares one
// static credential pointing at the given secret name/key — the shape that
// results from applying an AgentIdentity manifest before running setup.
func buildIdentityWithStaticCred(name, credName, secretName, secretKey string) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName, Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: secretKey},
				},
			}},
		},
	}
}

// TestStoreHonorsPredeclaredStaticSecretRef verifies that when a static
// credential is pre-declared with its own secretRef, Store writes the bytes
// to that declared name+key (honoring an empty key as the value-shape
// default), does NOT create the <identity>-<credential> convention secret,
// and leaves the credential's secretRef unchanged.
func TestStoreHonorsPredeclaredStaticSecretRef(t *testing.T) {
	cases := []struct {
		name        string
		declaredKey string // secretRef.Key as declared in YAML
		wantKey     string // key the bytes must land under
	}{
		{name: "explicit token key honored", declaredKey: "token", wantKey: "token"},
		{name: "custom key honored", declaredKey: "pat", wantKey: "pat"},
		{name: "empty key falls back to value-shape default", declaredKey: "", wantKey: "token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ai := buildIdentityWithStaticCred("pm-tools", "github-token", "gh-pat", tc.declaredKey)
			c := newClient(t, ai)

			req := setup.StoreRequest{
				Namespace:    "default",
				IdentityName: "pm-tools",
				Requirement:  authkind.CredentialRequirement{SuggestedName: "github-token"},
				Value:        builtins.StoreValue{Bearer: "ghp_new"},
			}
			require.NoError(t, setup.Store(ctx, c, req))

			// Bytes landed in the DECLARED secret under the resolved key.
			var sec corev1.Secret
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gh-pat"}, &sec))
			assert.Equal(t, "ghp_new", string(sec.Data[tc.wantKey]))

			// Convention-named secret was NOT created.
			var conv corev1.Secret
			err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pm-tools-github-token"}, &conv)
			assert.True(t, apierrors.IsNotFound(err), "convention secret must not be created")

			// Credential secretRef left as declared.
			var got spiceboxv1alpha1.AgentIdentity
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pm-tools"}, &got))
			require.Len(t, got.Spec.Credentials, 1)
			require.NotNil(t, got.Spec.Credentials[0].Static)
			assert.Equal(t, "gh-pat", got.Spec.Credentials[0].Static.SecretRef.Name)
			// The declared key field is left untouched too — including the
			// empty-key case, where Key stays "" even though bytes land under
			// the value-shape default ("token").
			assert.Equal(t, tc.declaredKey, got.Spec.Credentials[0].Static.SecretRef.Key,
				"declared secretRef.Key must be preserved")
		})
	}
}

// TestStoreHonorsPredeclaredOAuthSecretRef verifies that a pre-declared oauth
// credential's secretRef name is honored (oauth has no key field).
func TestStoreHonorsPredeclaredOAuthSecretRef(t *testing.T) {
	ctx := context.Background()
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-tools", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "linear-oauth", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: "linear-creds"},
				},
			}},
		},
	}
	c := newClient(t, ai)

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "pm-tools",
		Requirement:  authkind.CredentialRequirement{SuggestedName: "linear-oauth"},
		Value: builtins.StoreValue{OAuth: &builtins.OAuthValue{
			AccessToken: "at-1", RefreshToken: "rt-1",
		}},
	}
	require.NoError(t, setup.Store(ctx, c, req))

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "linear-creds"}, &sec))
	assert.Equal(t, "at-1", string(sec.Data["access_token"]))
	assert.Equal(t, "rt-1", string(sec.Data["refresh_token"]))

	var conv corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pm-tools-linear-oauth"}, &conv)
	assert.True(t, apierrors.IsNotFound(err), "convention secret must not be created")
}

// TestStoreFailsClosedOnTypeMismatch verifies that when a pre-declared
// credential's type disagrees with the produced value shape, Store returns a
// clear error and writes nothing, rather than silently writing mismatched
// keys.
func TestStoreFailsClosedOnTypeMismatch(t *testing.T) {
	cases := []struct {
		name     string
		identity *spiceboxv1alpha1.AgentIdentity
		value    builtins.StoreValue
		wantErr  string
	}{
		{
			name:     "declared static, produced oauth: fail-closed, nothing written",
			identity: buildIdentityWithStaticCred("pm-tools", "creds", "my-secret", "token"),
			value:    builtins.StoreValue{OAuth: &builtins.OAuthValue{AccessToken: "at-1"}},
			wantErr:  `type="static"`,
		},
		{
			name: "declared oauth, produced static: fail-closed, nothing written",
			identity: &spiceboxv1alpha1.AgentIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: "pm-tools", Namespace: "default"},
				Spec: spiceboxv1alpha1.AgentIdentitySpec{
					Credentials: []spiceboxv1alpha1.AgentCredential{{
						Name: "creds", Type: "oauth",
						OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
							SecretRef: spiceboxv1alpha1.SecretRef{Name: "my-secret"},
						},
					}},
				},
			},
			value:   builtins.StoreValue{Bearer: "ghp_x"},
			wantErr: `type="oauth"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newClient(t, tc.identity)
			req := setup.StoreRequest{
				Namespace:    "default",
				IdentityName: "pm-tools",
				Requirement:  authkind.CredentialRequirement{SuggestedName: "creds"},
				Value:        tc.value,
			}
			err := setup.Store(ctx, c, req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			// Nothing written: neither the declared nor the convention secret exists.
			var sec corev1.Secret
			assert.True(t, apierrors.IsNotFound(
				c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-secret"}, &sec)),
				"declared secret must not be written on type mismatch")
			var conv corev1.Secret
			assert.True(t, apierrors.IsNotFound(
				c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "pm-tools-creds"}, &conv)),
				"convention secret must not be written on type mismatch")
		})
	}
}

// TestStoreKubeconfigScaffolds verifies that Store creates everything from
// nothing for a kubeconfig-shaped credential (static, kubeconfig key).
func TestStoreKubeconfigScaffolds(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	kubeYAML := "apiVersion: v1\nkind: Config\nclusters: []\n"
	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "cluster-kube",
			BindingEnv:    map[string]string{"KUBECONFIG": ""},
		},
		Value: builtins.StoreValue{KubeconfigYAML: kubeYAML},
	}
	require.NoError(t, setup.Store(ctx, c, req))

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.Len(t, ai.Spec.Credentials, 1)
	cred := ai.Spec.Credentials[0]
	assert.Equal(t, "static", cred.Type)
	require.NotNil(t, cred.Static, "credential.Static nil")
	assert.Equal(t, "kubeconfig", cred.Static.SecretRef.Key)
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: cred.Static.SecretRef.Name}, &sec))
	assert.Equal(t, kubeYAML, string(sec.Data["kubeconfig"]))
	assert.NotNil(t, ai.Status.LastSetupAt, "LastSetupAt not set")
}

// TestStoreRecordsAttestation verifies that a SubjectID threaded in on
// StoreRequest lands on the credential Secret as both attestation
// annotations, keyed off the requirement's ProviderID.
func TestStoreRecordsAttestation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "gh-pat",
			ProviderID:    "github-pat",
		},
		Value:     builtins.StoreValue{Bearer: "ghp_xxxx"},
		SubjectID: "583231",
	}
	require.NoError(t, setup.Store(ctx, c, req))

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.Len(t, ai.Spec.Credentials, 1)

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].Static.SecretRef.Name}, &sec))
	assert.Equal(t, "github-pat", sec.Annotations[useridentity.AttestedProviderAnnotation])
	assert.Equal(t, "583231", sec.Annotations[useridentity.AttestedSubjectAnnotation])
}

// TestStoreWithoutSubjectIDWritesNoAttestation verifies that a requirement
// whose provider declares no subjectIDField — which surfaces here as an empty
// StoreRequest.SubjectID, since Store performs no verification of its own —
// stores the credential with neither attestation annotation, rather than
// writing a claim about an unknown account.
func TestStoreWithoutSubjectIDWritesNoAttestation(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "no-subject-provider",
			ProviderID:    "no-subject-provider",
		},
		Value: builtins.StoreValue{Bearer: "tok_xxxx"},
		// SubjectID intentionally left empty.
	}
	require.NoError(t, setup.Store(ctx, c, req))

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.Len(t, ai.Spec.Credentials, 1)

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].Static.SecretRef.Name}, &sec))
	assert.NotContains(t, sec.Annotations, useridentity.AttestedProviderAnnotation)
	assert.NotContains(t, sec.Annotations, useridentity.AttestedSubjectAnnotation)
}
