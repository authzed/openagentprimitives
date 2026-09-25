package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The per-kind descriptor builders (CLICredentialDescriptors,
// MCPCredentialDescriptor) were removed when the runtime moved to the
// shared, kind-agnostic credresolve.Descriptors path. Their resolution
// semantics are now covered by pkg/platform/identity/credresolve (Descriptors,
// remap, missing-credential error) and the per-kind requirement emission
// by the cli/mcp authkind SetupRequirements tests. What remains worth
// pinning here is the RuntimeIdentity projection the runner aliases expose.

// TestRuntimeIdentityFromAgentIdentity verifies the AgentIdentity
// projection: credentials carried verbatim, namespace from the AI, and a
// "<none>" label for a nil identity (the "no credentials" case the runner
// relies on for unauthenticated bundles/servers).
func TestRuntimeIdentityFromAgentIdentity(t *testing.T) {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "ai", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "linear", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "ai-linear"}},
			}},
		},
	}

	t.Run("projects credentials and namespace", func(t *testing.T) {
		ri := RuntimeIdentityFromAgentIdentity(ai)
		assert.Equal(t, "default", ri.Namespace)
		assert.Equal(t, "AgentIdentity ai", ri.Label)
		require.Len(t, ri.Credentials, 1)
		assert.Equal(t, "linear", ri.Credentials[0].Name)
	})

	t.Run("nil AgentIdentity yields no-credential label", func(t *testing.T) {
		ri := RuntimeIdentityFromAgentIdentity(nil)
		assert.Equal(t, "<none>", ri.Label)
		assert.Empty(t, ri.Credentials)
		assert.Empty(t, ri.Namespace)
	})
}

// TestRuntimeIdentityFromSessionUserIdentity verifies that
// SessionUserIdentity credentials project with the same shape as an
// AgentIdentity but resolve their Secret refs in IdentitiesNamespace, and
// that a nil SessionUserIdentity yields the "<none>" no-credential view.
func TestRuntimeIdentityFromSessionUserIdentity(t *testing.T) {
	suid := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-alice", Namespace: "default"},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: "sess-alice",
			UserIdentity: "alice",
			Subject:      "user:YWxpY2VAZXhhbXBsZS5jb20=",
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: "linear", Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "alice-linear"}},
			}},
		},
	}

	t.Run("projects credentials and uses IdentitiesNamespace", func(t *testing.T) {
		ri := RuntimeIdentityFromSessionUserIdentity(suid)
		assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, ri.Namespace)
		assert.Equal(t, "SessionUserIdentity sess-alice", ri.Label)
		require.Len(t, ri.Credentials, 1)
		assert.Equal(t, "linear", ri.Credentials[0].Name)
		// type=static credentials are redirected to the per-session projected
		// Secret in the session namespace; oauth/federated stay in Namespace.
		require.NotNil(t, ri.StaticProjection)
		assert.Equal(t, "default", ri.StaticProjection.Namespace)
		assert.Equal(t, spiceboxv1alpha1.PassthroughCredentialSecretName("sess-alice"), ri.StaticProjection.SecretName)
	})

	t.Run("nil SessionUserIdentity yields no-credential label", func(t *testing.T) {
		ri := RuntimeIdentityFromSessionUserIdentity(nil)
		assert.Equal(t, "<none>", ri.Label)
		assert.Empty(t, ri.Credentials)
		assert.Empty(t, ri.Namespace)
		assert.Nil(t, ri.StaticProjection)
	})
}
