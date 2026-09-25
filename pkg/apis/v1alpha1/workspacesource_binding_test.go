package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentClassWorkspaceSourceBinding(t *testing.T) {
	ac := AgentClass{Spec: AgentClassSpec{WorkspaceSource: &AgentClassWorkspaceSourceRef{Ref: "demo-source"}}}
	assert.Equal(t, "demo-source", ac.Spec.WorkspaceSource.Ref)

	var st AgentSessionStatus
	st.ResolvedWorkspaceSource = &ResolvedWorkspaceSource{Ref: "demo-source", BaseClaimName: "ws-base-demo-source", OverlayCut: true}
	assert.True(t, st.ResolvedWorkspaceSource.OverlayCut)
	assert.Equal(t, "ws-base-demo-source", st.ResolvedWorkspaceSource.BaseClaimName)
}
