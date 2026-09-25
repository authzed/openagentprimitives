package cli_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
)

func TestPrefix(t *testing.T) {
	assert.Equal(t, "cli", cli.New().Prefix(), "Prefix")
}

func TestResolveTargetEmbeddedToolkit(t *testing.T) {
	// "echo" is shipped as a built-in toolkit (toolkits/echo.yaml).
	tgt, err := cli.New().ResolveTarget(context.Background(), nil, "default", "echo")
	require.NoError(t, err, "ResolveTarget(echo)")
	assert.Equal(t, "echo", tgt.Name(), "Name")
	assert.Equal(t, "cli:echo", tgt.BindingMatchString(), "BindingMatchString")
}

func TestResolveTargetUnknown(t *testing.T) {
	_, err := cli.New().ResolveTarget(context.Background(), nil, "default", "definitely-not-a-toolkit")
	require.Error(t, err, "expected error")
	assert.True(t, errors.Is(err, authkind.ErrTargetNotFound), "errors.Is(ErrTargetNotFound)")
}

func TestSetupRequirementsFiltersSensitive(t *testing.T) {
	// "gh" toolkit declares GITHUB_TOKEN as sensitive (credential: github-token),
	// GH_HOST + HTTPS_PROXY as non-sensitive. We expect only GITHUB_TOKEN to surface.
	tgt, err := cli.New().ResolveTarget(context.Background(), nil, "default", "gh")
	require.NoError(t, err, "ResolveTarget(gh)")
	reqs := cli.New().SetupRequirements(context.Background(), tgt)
	require.Len(t, reqs, 1, "exactly one sensitive requirement")
	assert.Equal(t, "github-token", reqs[0].SuggestedName, "SuggestedName is the declared credential name")
	assert.Contains(t, reqs[0].BindingEnv, "GITHUB_TOKEN", "BindingEnv includes GITHUB_TOKEN")
	assert.Equal(t, "GITHUB_TOKEN", reqs[0].Inject.EnvVar, "Inject.EnvVar is the env var name")
	assert.Nil(t, reqs[0].Inject.Header, "Inject.Header unset for env-projected cli requirement")
	assert.False(t, reqs[0].IsBearer, "CLI requirement must not be IsBearer")
}

// TestResolveTargetTransientErrorDoesNotFallBackToEmbedded proves the authz
// boundary: a transient/RBAC error on the cluster SpiceboxToolkit Get must
// propagate, NOT silently fall through to the more-permissive embedded
// built-in. "echo" exists as an embedded toolkit, so a naive `err == nil`
// fall-through would mask the cluster CR (which an operator may have locked
// down) behind the embedded one.
func TestResolveTargetTransientErrorDoesNotFallBackToEmbedded(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewServiceUnavailable("transient API error")
			},
		}).Build()

	_, err := cli.New().ResolveTarget(context.Background(), c, "default", "echo")
	require.Error(t, err, "transient Get error must propagate, not fall back to embedded echo")
	assert.False(t, errors.Is(err, authkind.ErrTargetNotFound),
		"a transient error is NOT a not-found; must surface the real error")
}

// TestResolveTargetNotFoundFallsBackToEmbedded confirms the legitimate path is
// preserved: a genuine NotFound on the cluster CR falls back to the embedded
// built-in toolkit.
func TestResolveTargetNotFoundFallsBackToEmbedded(t *testing.T) {
	// Empty fake client → Get returns NotFound; "echo" resolves from embedded.
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	tgt, err := cli.New().ResolveTarget(context.Background(), c, "default", "echo")
	require.NoError(t, err, "NotFound on cluster CR ⇒ embedded fallback")
	assert.Equal(t, "echo", tgt.Name())
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// TestSetupRequirementsUsesExplicitCredentialNoFallback proves SetupRequirements
// reads the env's declared credential name verbatim and does NOT infer a name
// from the env. The fixture's credential ("my-cred") deliberately differs from
// any lowercased form of "MY_API_KEY" (e.g. "my-api-key").
func TestSetupRequirementsUsesExplicitCredentialNoFallback(t *testing.T) {
	sbtk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: "custom"},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            "custom",
			Version:         "1.0.0",
			ToolkitRevision: "1",
			Target:          spiceboxv1alpha1.ToolkitTarget{Binary: "custom"},
			Parser:          spiceboxv1alpha1.ToolkitParserConfig{Kind: "declarative"},
			Env: spiceboxv1alpha1.ToolkitEnv{
				Allowed: []spiceboxv1alpha1.ToolkitEnvVar{
					{Name: "MY_API_KEY", Sensitive: true, Provider: "custom-app", Credential: "my-cred"},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sbtk).Build()
	tgt, err := cli.New().ResolveTarget(context.Background(), c, "default", "custom")
	require.NoError(t, err, "ResolveTarget(custom)")

	reqs := cli.New().SetupRequirements(context.Background(), tgt)
	require.Len(t, reqs, 1, "exactly one sensitive requirement")
	assert.Equal(t, "my-cred", reqs[0].SuggestedName, "SuggestedName is the explicit credential, not a lowercased env name")
	assert.NotEqual(t, "my-api-key", reqs[0].SuggestedName, "must NOT infer a lowercased name from MY_API_KEY")
	assert.Equal(t, "MY_API_KEY", reqs[0].Inject.EnvVar, "Inject.EnvVar is the env var name")
	assert.Equal(t, "custom-app", reqs[0].ProviderID, "ProviderID carried from env.provider")
}
