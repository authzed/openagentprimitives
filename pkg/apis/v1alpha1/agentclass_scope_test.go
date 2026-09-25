package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestGetScope_NilAuthz(t *testing.T) {
	s := &v1.AgentClassSpec{}
	scope := s.GetScope()
	assert.NotNil(t, scope)
	assert.False(t, scope.Enabled)
}

func TestGetScope_NilScope(t *testing.T) {
	s := &v1.AgentClassSpec{Authz: &v1.AuthzBlock{}}
	scope := s.GetScope()
	assert.NotNil(t, scope)
	assert.False(t, scope.Enabled)
}

func TestGetScope_Set(t *testing.T) {
	s := &v1.AgentClassSpec{Authz: &v1.AuthzBlock{Scope: &v1.ScopeSpec{Enabled: true, ColdStart: "extractAndApprove"}}}
	scope := s.GetScope()
	assert.True(t, scope.Enabled)
	assert.Equal(t, "extractAndApprove", scope.ColdStart)
}

func TestScopeSpec_ColdStartField(t *testing.T) {
	s := &v1.AgentClassSpec{Authz: &v1.AuthzBlock{Scope: &v1.ScopeSpec{Enabled: true, ColdStart: "off"}}}
	assert.Equal(t, "off", s.GetScope().ColdStart)
}
