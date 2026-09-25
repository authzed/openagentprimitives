package sandboxkinds_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// domainKind is a stand-in Kind with a controllable workspace domain. It is
// never registered — CheckWorkspaceDomains takes already-resolved Kinds, so
// these fixtures are built and passed directly.
type domainKind struct {
	name   string
	domain string
}

func (d domainKind) Name() string                                 { return d.name }
func (domainKind) Supports(sandboxkinds.Feature) bool             { return true }
func (d domainKind) WorkspaceDomain() string                      { return d.domain }
func (domainKind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }
func (domainKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, nil
}

var (
	kindK8sA      = domainKind{name: "k8s-a", domain: sandboxkinds.DomainKubernetesPVC}
	kindK8sB      = domainKind{name: "k8s-b", domain: sandboxkinds.DomainKubernetesPVC}
	kindElsewhere = domainKind{name: "elsewhere", domain: "vendor-volume"}
	kindNoSharing = domainKind{name: "no-sharing", domain: ""}
)

func TestCheckWorkspaceDomains(t *testing.T) {
	cases := []struct {
		name      string
		resolved  map[string]sandboxkinds.Kind
		shared    bool
		hasSource bool
		wantErr   string
	}{
		{
			name:     "isolated workspaces: domains are irrelevant, anything goes",
			resolved: map[string]sandboxkinds.Kind{"a": kindK8sA, "b": kindElsewhere},
			shared:   false,
		},
		{
			name:     "shared, one backend: accepted",
			resolved: map[string]sandboxkinds.Kind{"a": kindK8sA, "b": kindK8sA},
			shared:   true,
		},
		{
			name:     "shared, two backends in the same domain: accepted",
			resolved: map[string]sandboxkinds.Kind{"a": kindK8sA, "b": kindK8sB},
			shared:   true,
		},
		{
			name:     "shared across different domains: refused, naming both",
			resolved: map[string]sandboxkinds.Kind{"a": kindK8sA, "b": kindElsewhere},
			shared:   true,
			wantErr:  "elsewhere",
		},
		{
			name:     "shared with a backend that cannot share at all: refused",
			resolved: map[string]sandboxkinds.Kind{"a": kindNoSharing, "b": kindNoSharing},
			shared:   true,
			wantErr:  "cannot share",
		},
		{
			name:      "a workspace source forces the kubernetes domain",
			resolved:  map[string]sandboxkinds.Kind{"a": kindElsewhere},
			shared:    true,
			hasSource: true,
			wantErr:   "workspace source",
		},
		{
			name:      "a workspace source with a kubernetes backend: accepted",
			resolved:  map[string]sandboxkinds.Kind{"a": kindK8sA},
			shared:    true,
			hasSource: true,
		},
		{
			name:      "a workspace source forces the kubernetes domain even without a shared workspace: refused",
			resolved:  map[string]sandboxkinds.Kind{"a": kindElsewhere},
			shared:    false,
			hasSource: true,
			wantErr:   "workspace source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sandboxkinds.CheckWorkspaceDomains(tc.resolved, tc.shared, tc.hasSource)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
