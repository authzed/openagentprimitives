package identitycmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func TestUpsertCredential(t *testing.T) {
	static := func(name, secretName string) spiceboxv1alpha1.AgentCredential {
		return spiceboxv1alpha1.AgentCredential{
			Name: name, Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
			},
		}
	}

	existing := []spiceboxv1alpha1.AgentCredential{
		static("github", "u-abc-github"),
		static("slack", "u-abc-slack"),
	}

	cases := []struct {
		name       string
		initial    []spiceboxv1alpha1.AgentCredential
		incoming   spiceboxv1alpha1.AgentCredential
		wantLen    int
		wantAt     int
		wantSecret string
	}{
		{
			name:       "add new credential: appends to the list",
			initial:    existing,
			incoming:   static("jira", "u-abc-jira"),
			wantLen:    3,
			wantAt:     2,
			wantSecret: "u-abc-jira",
		},
		{
			name:       "replace existing credential by name: updates in place",
			initial:    existing,
			incoming:   static("github", "u-abc-github-v2"),
			wantLen:    2,
			wantAt:     0,
			wantSecret: "u-abc-github-v2",
		},
		{
			name:       "replace last credential by name: updates in place",
			initial:    existing,
			incoming:   static("slack", "u-abc-slack-new"),
			wantLen:    2,
			wantAt:     1,
			wantSecret: "u-abc-slack-new",
		},
		{
			name:       "add to empty list: produces single-element list",
			initial:    nil,
			incoming:   static("github", "u-abc-github"),
			wantLen:    1,
			wantAt:     0,
			wantSecret: "u-abc-github",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// upsertCredential must not mutate the original slice in the replace path.
			initialCopy := make([]spiceboxv1alpha1.AgentCredential, len(tc.initial))
			copy(initialCopy, tc.initial)

			got := upsertCredential(initialCopy, tc.incoming)
			assert.Len(t, got, tc.wantLen, "result length")
			assert.Equal(t, tc.incoming.Name, got[tc.wantAt].Name, "credential name at expected position")
			assert.Equal(t, tc.wantSecret, got[tc.wantAt].Static.SecretRef.Name, "secret name at expected position")
		})
	}
}

// TestRunUserIdentityPutToken_ValidatesFormat — `oap user-identity put-token`
// validates the token against the credential's declared provider format
// before writing anything. The credential "anthropic-oauth" resolves to the
// anthropic-oauth provider via the embedded toolkit catalog, so a fake client
// with no seeded objects is enough. A wrong-format token must error and write
// nothing; a correctly-shaped one must persist.
func TestRunUserIdentityPutToken_ValidatesFormat(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, fn := range []func(*runtime.Scheme) error{corev1.AddToScheme, spiceboxv1alpha1.AddToScheme} {
		require.NoError(t, fn(scheme), "AddToScheme")
	}
	const (
		subject  = "user:alice@example.com"
		credName = "anthropic-oauth"
	)
	uiKey := client.ObjectKey{Name: useridentity.NameForSubject(subject)}

	t.Run("wrong-format token: errors and writes nothing", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		var out bytes.Buffer
		err := runUserIdentityPutToken(context.Background(), &out, c, subject, credName, "zkeI-not-an-oat-token", "", false)
		require.Error(t, err, "a wrong-format token must be refused")
		assert.Contains(t, err.Error(), "sk-ant-oat", "the error must surface the expected format")

		var ui spiceboxv1alpha1.UserIdentity
		assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), uiKey, &ui)),
			"no UserIdentity may be written for a rejected token")
	})

	t.Run("correctly-formatted token: stored", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		var out bytes.Buffer
		require.NoError(t, runUserIdentityPutToken(context.Background(), &out, c, subject, credName, "sk-ant-oat01-GOOD", "", false),
			"a correctly-formatted token must store")

		var ui spiceboxv1alpha1.UserIdentity
		require.NoError(t, c.Get(context.Background(), uiKey, &ui), "UserIdentity must be created")
		require.Len(t, ui.Spec.Credentials, 1)
		assert.Equal(t, credName, ui.Spec.Credentials[0].Name)
	})
}
