package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// toolCallWithSources builds a ToolCall in namespace "default" owned by the
// named AgentSession (none when owner == "") with one credential per source
// name. All sources are same-namespace so they pass the namespace check and the
// ownership check is exercised in isolation.
func toolCallWithSources(t *testing.T, owner string, sourceNames ...string) *ToolCall {
	t.Helper()
	tc := &ToolCall{}
	tc.Namespace = "default"
	if owner != "" {
		tc.OwnerReferences = []metav1.OwnerReference{{Kind: "AgentSession", Name: owner}}
	}
	for _, n := range sourceNames {
		tc.Spec.Credentials = append(tc.Spec.Credentials, CredentialDescriptor{
			Source: CredentialSource{Type: "static", Namespace: "default", Name: n},
		})
	}
	return tc
}

func TestValidateCredentialSourceOwnership(t *testing.T) {
	cases := []struct {
		name        string
		owner       string
		sourceNames []string
		wantErr     bool
	}{
		{
			name:        "own passthrough Secret: allowed",
			owner:       "session-alice",
			sourceNames: []string{PassthroughCredentialSecretName("session-alice")},
			wantErr:     false,
		},
		{
			name:        "sibling session's passthrough Secret: refused (cross-session)",
			owner:       "session-alice",
			sourceNames: []string{PassthroughCredentialSecretName("session-bob")},
			wantErr:     true,
		},
		{
			name:        "non-passthrough Secret: unaffected (allowed)",
			owner:       "session-alice",
			sourceNames: []string{"some-toolkit-secret"},
			wantErr:     false,
		},
		{
			name:        "own passthrough + an unrelated Secret: allowed",
			owner:       "session-alice",
			sourceNames: []string{PassthroughCredentialSecretName("session-alice"), "git-token"},
			wantErr:     false,
		},
		{
			name:        "passthrough Secret but no AgentSession owner: refused (nothing to bind to)",
			owner:       "",
			sourceNames: []string{PassthroughCredentialSecretName("session-alice")},
			wantErr:     true,
		},
		{
			name:        "no credentials: allowed",
			owner:       "session-alice",
			sourceNames: nil,
			wantErr:     false,
		},
		{
			name:        "own memory-token Secret: allowed",
			owner:       "session-alice",
			sourceNames: []string{"session-alice" + MemoryTokenSecretSuffix},
			wantErr:     false,
		},
		{
			name:        "sibling session's memory-token Secret: refused (cross-session audit-key theft)",
			owner:       "session-alice",
			sourceNames: []string{"session-bob" + MemoryTokenSecretSuffix},
			wantErr:     true,
		},
		{
			name:        "own secret-output Secret: allowed",
			owner:       "session-alice",
			sourceNames: []string{"session-alice" + SecretOutputSecretSuffix},
			wantErr:     false,
		},
		{
			name:        "sibling session's secret-output Secret: refused (cross-session)",
			owner:       "session-alice",
			sourceNames: []string{"session-bob" + SecretOutputSecretSuffix},
			wantErr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := toolCallWithSources(t, tc.owner, tc.sourceNames...).ValidateCredentialSourceOwnership()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.sourceNames[0], "error should name the offending per-session Secret")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestPerSessionSecretSuffixesIncludesKnownKinds pins the membership of the
// canonical per-session suffix list: every known per-session Secret kind
// (passthrough credentials, memory-token, secret-output) must be present, or
// ValidateCredentialSourceOwnership silently fails to bind that kind to its
// owning session.
func TestPerSessionSecretSuffixesIncludesKnownKinds(t *testing.T) {
	assert.Contains(t, PerSessionSecretSuffixes, PassthroughCredentialSecretSuffix)
	assert.Contains(t, PerSessionSecretSuffixes, MemoryTokenSecretSuffix)
	assert.Contains(t, PerSessionSecretSuffixes, SecretOutputSecretSuffix)
}

// TestValidateCredentialSourceOwnership_NamesTheOwningSession pins the
// actionable wording: a cross-session reference names both the offending Secret
// and the only Secret this ToolCall may reference.
func TestValidateCredentialSourceOwnership_NamesTheOwningSession(t *testing.T) {
	err := toolCallWithSources(t, "session-alice", PassthroughCredentialSecretName("session-bob")).
		ValidateCredentialSourceOwnership()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session-bob-passthrough-creds")
	assert.Contains(t, err.Error(), PassthroughCredentialSecretName("session-alice"))
}
