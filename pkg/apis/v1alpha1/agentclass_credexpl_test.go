package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentClassCredentialExplanations(t *testing.T) {
	ac := AgentClass{Spec: AgentClassSpec{
		CredentialExplanations: []CredentialExplanationSpec{
			{Credential: "github-token", Reason: "So coder-bot can push commits as you."},
		},
	}}
	require.Len(t, ac.Spec.CredentialExplanations, 1)
	assert.Equal(t, "github-token", ac.Spec.CredentialExplanations[0].Credential)
	assert.Equal(t, "So coder-bot can push commits as you.", ac.Spec.CredentialExplanations[0].Reason)

	// DeepCopy must copy the slice (proves controller-gen regen ran).
	cp := ac.DeepCopy()
	cp.Spec.CredentialExplanations[0].Reason = "changed"
	assert.Equal(t, "So coder-bot can push commits as you.", ac.Spec.CredentialExplanations[0].Reason,
		"DeepCopy must not alias the slice")
}
