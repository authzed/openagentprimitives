// pkg/settings/resolve_standing_test.go
package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestResolveRequireStandingFor(t *testing.T) {
	limits := func(types ...string) *v1.SettingsSpec {
		return &v1.SettingsSpec{Limits: &v1.SettingsLimits{RequireStandingFor: types}}
	}
	cases := []struct {
		name             string
		cluster, namespc *v1.SettingsSpec
		want             []string
	}{
		{name: "no tier sets a veto: empty", want: nil},
		{name: "cluster only: cluster's set", cluster: limits("crm_company"), want: []string{"crm_company"}},
		{
			name:    "namespace ADDS to cluster, never removes",
			cluster: limits("crm_company"), namespc: limits("git_repo"),
			want: []string{"crm_company", "git_repo"},
		},
		{
			name:    "namespace naming a subset does NOT drop the cluster's entries",
			cluster: limits("crm_company", "git_repo"), namespc: limits("git_repo"),
			want: []string{"crm_company", "git_repo"},
		},
		{
			name:    "duplicates collapse and the result is sorted",
			cluster: limits("git_repo", "crm_company"), namespc: limits("git_repo"),
			want: []string{"crm_company", "git_repo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRequireStandingFor(Inputs{Cluster: tc.cluster, Namespace: tc.namespc})
			assert.Equal(t, tc.want, got)
		})
	}
}
