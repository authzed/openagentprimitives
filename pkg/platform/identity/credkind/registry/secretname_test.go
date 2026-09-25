package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind.Kinds against THIS test
	// binary's registry. Without this blank import registry.SecretNameFor
	// would fail closed on every case below regardless of what any other
	// package imports — a test binary is its own process.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry/registrytest"
)

func staticCred(name, secret, key string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secret, Key: key}}}
}

func oauthCred(name, secret string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{Name: name, Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: secret}}}
}

// TestSecretNameFor_AnswersForEveryRegisteredKind pins the three distinct
// outcomes SecretNameFor must produce: a name for a type with a backing
// Secret, ("", nil) for a registered type with none (federated), and an
// error — never a guessed name — for a type the registry does not know.
func TestSecretNameFor_AnswersForEveryRegisteredKind(t *testing.T) {
	registrytest.Snapshot(t)
	require.NotEmpty(t, registry.Keys(),
		"registry is empty — this test binary is missing its blank import of credkind/imports")

	cases := []struct {
		name     string
		cred     spiceboxv1alpha1.AgentCredential
		wantName string
		wantErr  bool
	}{
		{
			name:     "static: names its Secret",
			cred:     staticCred("c", "demo-static", "token"),
			wantName: "demo-static",
		},
		{
			name:     "oauth: names its Secret despite the empty Key",
			cred:     oauthCred("c", "demo-oauth"),
			wantName: "demo-oauth",
		},
		{
			name:     "federated: no backing Secret, returns empty name and no error",
			cred:     spiceboxv1alpha1.AgentCredential{Name: "c", Type: "federated"},
			wantName: "",
		},
		{
			name:    "unregistered type: returns an error rather than guessing",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "nosuch"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := registry.SecretNameFor(tc.cred)
			if tc.wantErr {
				assert.Error(t, err)
				assert.Empty(t, got, "an error case must not also hand back a name to use")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantName, got)
		})
	}
}

// TestSecretNameFor_UnregisteredTypeErrorNamesTheType asserts the error
// message itself is diagnosable — a caller logging it must be able to see
// which type was unrecognized without source-diving into the registry.
func TestSecretNameFor_UnregisteredTypeErrorNamesTheType(t *testing.T) {
	registrytest.Snapshot(t)
	require.NotEmpty(t, registry.Keys(),
		"registry is empty — this test binary is missing its blank import of credkind/imports")

	_, err := registry.SecretNameFor(spiceboxv1alpha1.AgentCredential{Name: "c", Type: "nosuch"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nosuch")
}
