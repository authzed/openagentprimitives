package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGitHubAppCredential_RoundTripsThroughDeepCopy asserts that
// GitHubAppCredentialSource is wired into AgentCredential's generated
// DeepCopyInto: it must survive a DeepCopy and the copy must not alias the
// original's pointer. If GitHubApp were dropped from
// zz_generated.deepcopy.go's DeepCopyInto for AgentCredential — the exact
// class of miss a hand-edited deepcopy stanza invites — out.GitHubApp would
// come back nil, or would alias in.GitHubApp and the mutation assertion below
// would fail.
func TestGitHubAppCredential_RoundTripsThroughDeepCopy(t *testing.T) {
	in := AgentCredential{
		Name:      "gh-app",
		Type:      "githubApp",
		GitHubApp: &GitHubAppCredentialSource{SecretRef: SecretRef{Name: "demo-reviewbot-github-app"}},
	}
	out := in.DeepCopy()
	require.NotNil(t, out.GitHubApp, "GitHubApp must survive deepcopy")
	assert.Equal(t, "demo-reviewbot-github-app", out.GitHubApp.SecretRef.Name)

	out.GitHubApp.SecretRef.Name = "mutated"
	assert.Equal(t, "demo-reviewbot-github-app", in.GitHubApp.SecretRef.Name,
		"deepcopy must not alias the original")
}
