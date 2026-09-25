package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// stubKind is a minimal idp.Kind for use in tests only.
type stubKind struct{ name string }

func (s *stubKind) Name() string { return s.name }
func (s *stubKind) New(_ context.Context, _ idp.Config) (idp.Provider, error) {
	return nil, nil
}
func (s *stubKind) Wizard() idp.Wizard { return nil }
func (s *stubKind) ValidateSpec(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}
func (s *stubKind) DiscoveryURL(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}
func (s *stubKind) AllowedNonLocal() bool { return true }

func TestRegistry(t *testing.T) {
	cases := []struct {
		name     string
		register []string
		get      string
		wantHit  bool
	}{
		{
			name:     "Get hit: registered kind is returned",
			register: []string{"alpha"},
			get:      "alpha",
			wantHit:  true,
		},
		{
			name:     "Get miss: unregistered name returns false",
			register: []string{"alpha"},
			get:      "beta",
			wantHit:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry.Reset()
			for _, n := range tc.register {
				registry.Register(&stubKind{name: n})
			}
			got, ok := registry.Get(tc.get)
			assert.Equal(t, tc.wantHit, ok)
			if tc.wantHit {
				require.NotNil(t, got)
				assert.Equal(t, tc.get, got.Name())
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	registry.Reset()
	registry.Register(&stubKind{name: "dup"})
	assert.Panics(t, func() {
		registry.Register(&stubKind{name: "dup"})
	}, "duplicate registration must panic")
}

func TestNames(t *testing.T) {
	registry.Reset()
	registry.Register(&stubKind{name: "zeta"})
	registry.Register(&stubKind{name: "alpha"})
	names := registry.Names()
	require.Equal(t, []string{"alpha", "zeta"}, names, "Names() must be sorted")
}
