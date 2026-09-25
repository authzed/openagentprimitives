package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The helper is what the AgentSession reconciler levels the
// agentsession#artifact_org_viewer wildcard tuple from, so its false cases are
// as load-bearing as its true one: anything that is not an explicit
// "organization" opt-in must read as session-only.
func TestSessionAuthzOrgWideArtifactVisibility(t *testing.T) {
	cases := []struct {
		name string
		spec AgentClassSpec
		want bool
	}{
		{name: "no authz block at all: session-only", spec: AgentClassSpec{}, want: false},
		{name: "authz block without session: session-only", spec: AgentClassSpec{Authz: &AuthzBlock{}}, want: false},
		{name: "empty visibility: session-only (the default)", spec: AgentClassSpec{Authz: &AuthzBlock{Session: &SessionAuthz{}}}, want: false},
		{name: "explicit session: session-only", spec: AgentClassSpec{Authz: &AuthzBlock{Session: &SessionAuthz{ArtifactVisibility: ArtifactVisibilitySession}}}, want: false},
		{name: "organization: org-wide", spec: AgentClassSpec{Authz: &AuthzBlock{Session: &SessionAuthz{ArtifactVisibility: ArtifactVisibilityOrganization}}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.spec.GetAuthz().GetSession().OrgWideArtifactVisibility())
		})
	}
}

// A nil *SessionAuthz receiver must be safe: callers reach the helper through
// chains that may hand back a nil typed pointer.
func TestSessionAuthzOrgWideArtifactVisibility_NilReceiver(t *testing.T) {
	var s *SessionAuthz
	assert.False(t, s.OrgWideArtifactVisibility())
}
