package identitycmd

import (
	"bytes"
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
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func newUserIdentityDeleteTokenScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, fn := range []func(*runtime.Scheme) error{corev1.AddToScheme, spiceboxv1alpha1.AddToScheme} {
		require.NoError(t, fn(s), "AddToScheme")
	}
	return s
}

func staticCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name, Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
		},
	}
}

func masterSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"token": []byte("tok")},
	}
}

// TestRemoveCredential covers the pure slice-mutation contract: which entry is
// dropped, the present/absent signal, and that the original slice is untouched.
func TestRemoveCredential(t *testing.T) {
	base := []spiceboxv1alpha1.AgentCredential{
		staticCred("github-pat", "u-x-github-pat"),
		staticCred("anthropic-oauth", "u-x-anthropic-oauth"),
		staticCred("slack", "u-x-slack"),
	}

	cases := []struct {
		name      string
		remove    string
		wantFound bool
		wantNames []string
	}{
		{
			name:      "remove middle credential: drops it, keeps siblings in order",
			remove:    "anthropic-oauth",
			wantFound: true,
			wantNames: []string{"github-pat", "slack"},
		},
		{
			name:      "remove first credential: drops it, keeps the rest",
			remove:    "github-pat",
			wantFound: true,
			wantNames: []string{"anthropic-oauth", "slack"},
		},
		{
			name:      "remove absent credential: found=false, list unchanged",
			remove:    "nope",
			wantFound: false,
			wantNames: []string{"github-pat", "anthropic-oauth", "slack"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := make([]spiceboxv1alpha1.AgentCredential, len(base))
			copy(input, base)

			got, found := removeCredential(input, tc.remove)
			assert.Equal(t, tc.wantFound, found, "found signal")

			var gotNames []string
			for _, c := range got {
				gotNames = append(gotNames, c.Name)
			}
			assert.Equal(t, tc.wantNames, gotNames, "remaining credential names")

			// The original slice must not be mutated.
			assert.Equal(t, len(base), len(input), "input slice length unchanged")
			assert.Equal(t, base[0].Name, input[0].Name, "input slice contents unchanged")
		})
	}
}

func TestRunUserIdentityDeleteToken_RemovesEntryAndSecretKeepsSiblings(t *testing.T) {
	ctx := context.Background()
	scheme := newUserIdentityDeleteTokenScheme(t)
	const uiName = "u-bob"

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: uiName},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:Ym9iQGV4YW1wbGUuY29t",
			Credentials: []spiceboxv1alpha1.AgentCredential{
				staticCred("github-pat", useridentity.MasterSecretName(uiName, "github-pat")),
				staticCred("anthropic-oauth", useridentity.MasterSecretName(uiName, "anthropic-oauth")),
			},
		},
	}
	targetSecret := masterSecret(useridentity.MasterSecretName(uiName, "anthropic-oauth"))
	siblingSecret := masterSecret(useridentity.MasterSecretName(uiName, "github-pat"))
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ui, targetSecret, siblingSecret).Build()

	var out bytes.Buffer
	require.NoError(t, runUserIdentityDeleteToken(ctx, &out, c, uiName, "anthropic-oauth"),
		"delete-token of an existing credential should succeed")

	// Spec entry removed, sibling intact.
	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &got), "get UserIdentity")
	require.Len(t, got.Spec.Credentials, 1, "only the sibling credential remains")
	assert.Equal(t, "github-pat", got.Spec.Credentials[0].Name, "surviving credential name")

	// Target Secret deleted; the computed name is u-bob-anthropic-oauth.
	err := c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      "u-bob-anthropic-oauth",
	}, &corev1.Secret{})
	assert.True(t, apierrors.IsNotFound(err), "target master Secret should be deleted")

	// Sibling Secret untouched.
	require.NoError(t, c.Get(ctx, client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      "u-bob-github-pat",
	}, &corev1.Secret{}), "sibling master Secret should remain")

	assert.Contains(t, out.String(), "Connect your accounts", "re-link hint surfaced")
}

func TestRunUserIdentityDeleteToken_MissingCredentialErrors(t *testing.T) {
	ctx := context.Background()
	scheme := newUserIdentityDeleteTokenScheme(t)
	const uiName = "u-alice"

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: uiName},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:YWxpY2VAZXhhbXBsZS5jb20=",
			Credentials: []spiceboxv1alpha1.AgentCredential{staticCred("github-pat", useridentity.MasterSecretName(uiName, "github-pat"))},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui).Build()

	var out bytes.Buffer
	err := runUserIdentityDeleteToken(ctx, &out, c, uiName, "anthropic-oauth")
	require.Error(t, err, "deleting a credential that is not present must error, not no-op")
	assert.Contains(t, err.Error(), "no credential named", "error explains the missing credential")

	// Spec must be left untouched on the refuse path.
	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &got), "get UserIdentity")
	assert.Len(t, got.Spec.Credentials, 1, "credential list unchanged on refuse")
}

func TestRunUserIdentityDeleteToken_MissingSecretStillRemovesEntry(t *testing.T) {
	ctx := context.Background()
	scheme := newUserIdentityDeleteTokenScheme(t)
	const uiName = "u-carol"

	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: uiName},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:Y2Fyb2xAZXhhbXBsZS5jb20=",
			Credentials: []spiceboxv1alpha1.AgentCredential{staticCred("anthropic-oauth", useridentity.MasterSecretName(uiName, "anthropic-oauth"))},
		},
	}
	// No backing Secret seeded — Delete should treat NotFound as already-absent.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ui).Build()

	var out bytes.Buffer
	require.NoError(t, runUserIdentityDeleteToken(ctx, &out, c, uiName, "anthropic-oauth"),
		"absent backing Secret should not fail the removal")

	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &got), "get UserIdentity")
	assert.Empty(t, got.Spec.Credentials, "credential removed even though its Secret was absent")
	assert.Contains(t, out.String(), "already absent", "absent-Secret path surfaced")
}

func TestRunUserIdentityDeleteToken_MissingIdentityErrors(t *testing.T) {
	ctx := context.Background()
	scheme := newUserIdentityDeleteTokenScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	var out bytes.Buffer
	err := runUserIdentityDeleteToken(ctx, &out, c, "u-missing", "github-pat")
	require.Error(t, err, "unknown UserIdentity must error")
	assert.Contains(t, err.Error(), "get UserIdentity", "error mentions the failing get")
}
