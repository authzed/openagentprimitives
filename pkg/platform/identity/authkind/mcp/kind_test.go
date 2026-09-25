package mcp_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// resolveTarget is the shared fixture: build an MCPServer, install it
// into a fake client, resolve the target. Returns the kind + target.
func resolveTarget(t *testing.T, srv *spiceboxv1alpha1.MCPServer) (authkind.Kind, authkind.Target) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(srv).Build()
	k := mcpkind.New()
	tgt, err := k.ResolveTarget(context.Background(), c, srv.Namespace, srv.Name)
	require.NoError(t, err, "ResolveTarget")
	return k, tgt
}

func TestPrefix(t *testing.T) {
	assert.Equal(t, "mcp", mcpkind.New().Prefix(), "Prefix")
}

func TestResolveAndIntent(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{}
	srv.Name = "linear"
	srv.Namespace = "default"
	srv.Spec.Intent = "Read Linear issues"
	srv.Spec.Auth.Provider = "oauth-mcp"
	_, tgt := resolveTarget(t, srv)
	assert.Equal(t, "mcp:linear", tgt.BindingMatchString(), "BindingMatchString")
	assert.Equal(t, "Read Linear issues", tgt.Intent(), "Intent")
}

func TestSetupRequirements(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{}
	srv.Name = "linear"
	srv.Namespace = "default"
	srv.Spec.Auth.Provider = "oauth-mcp"
	srv.Spec.Auth.Credential = "linear-token"
	k, tgt := resolveTarget(t, srv)
	reqs := k.SetupRequirements(context.Background(), tgt)
	require.Len(t, reqs, 1, "exactly one requirement")
	assert.Equal(t, "linear-token", reqs[0].SuggestedName, "SuggestedName from CredentialNameForServer")
	assert.True(t, reqs[0].IsBearer, "IsBearer")
	assert.Equal(t, "oauth-mcp", reqs[0].ProviderID, "ProviderID")
	require.NotNil(t, reqs[0].Inject.Header, "Inject.Header set for header-projected mcp requirement")
	assert.Empty(t, reqs[0].Inject.EnvVar, "Inject.EnvVar unset for mcp requirement")
	assert.Equal(t, "Authorization", reqs[0].Inject.Header.Name, "default header name")
	assert.Equal(t, "Bearer ", reqs[0].Inject.Header.ValuePrefix, "default bearer prefix when no custom header")
}

// TestSetupRequirementsUnauthenticatedReturnsNil proves an MCPServer with no
// declared credential (CredentialNameForServer == "") yields no requirements.
func TestSetupRequirementsUnauthenticatedReturnsNil(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{}
	srv.Name = "public"
	srv.Namespace = "default"
	// No Spec.Auth.Credential → unauthenticated.
	k, tgt := resolveTarget(t, srv)
	reqs := k.SetupRequirements(context.Background(), tgt)
	assert.Nil(t, reqs, "unauthenticated server emits no requirement")
}

func TestSetupRequirementsCustomHeader(t *testing.T) {
	// A custom header with no ValuePrefix → no default "Bearer " is added.
	srv := &spiceboxv1alpha1.MCPServer{}
	srv.Name = "linear"
	srv.Namespace = "default"
	srv.Spec.Auth.Provider = "oauth-mcp"
	srv.Spec.Auth.Credential = "linear-token"
	srv.Spec.Auth.Header = "X-Api-Key"
	k, tgt := resolveTarget(t, srv)
	reqs := k.SetupRequirements(context.Background(), tgt)
	require.Len(t, reqs, 1, "exactly one requirement")
	require.NotNil(t, reqs[0].Inject.Header, "Inject.Header set")
	assert.Equal(t, "X-Api-Key", reqs[0].Inject.Header.Name, "custom header name")
	assert.Empty(t, reqs[0].Inject.Header.ValuePrefix, "no default prefix for a custom header")
}

func TestSetupRequirementsCustomHeaderWithPrefix(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{}
	srv.Name = "linear"
	srv.Namespace = "default"
	srv.Spec.Auth.Provider = "oauth-mcp"
	srv.Spec.Auth.Credential = "linear-token"
	srv.Spec.Auth.Header = "X-Token"
	srv.Spec.Auth.ValuePrefix = "Token "
	k, tgt := resolveTarget(t, srv)
	reqs := k.SetupRequirements(context.Background(), tgt)
	require.Len(t, reqs, 1, "exactly one requirement")
	require.NotNil(t, reqs[0].Inject.Header, "Inject.Header set")
	assert.Equal(t, "X-Token", reqs[0].Inject.Header.Name, "custom header name")
	assert.Equal(t, "Token ", reqs[0].Inject.Header.ValuePrefix, "explicit ValuePrefix preserved")
}
