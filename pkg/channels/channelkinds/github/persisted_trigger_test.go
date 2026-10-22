package github

import (
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPersistedWorkflowTriggerValidation(t *testing.T) {
	valid := func() *api.Channel {
		return &api.Channel{Spec: api.ChannelSpec{Kind: "github", Role: api.ChannelRoleInput, GitHub: &api.GitHubChannelConfig{Repositories: []string{"synthetic/widgets"}, Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/check.yml"}}}}}}
	}
	require.NoError(t, (Kind{}).ValidateSpec(valid()))
	cases := []struct {
		name   string
		mutate func(*api.GitHubChannelConfig)
	}{
		{"missing workflows", func(c *api.GitHubChannelConfig) { c.Trigger.Workflows = nil }},
		{"unknown event", func(c *api.GitHubChannelConfig) { c.Trigger.Event = "push" }},
		{"unknown action", func(c *api.GitHubChannelConfig) { c.Trigger.Actions = []string{"requested"} }},
		{"unknown readiness", func(c *api.GitHubChannelConfig) { c.Trigger.Readiness = "successful" }},
		{"missing repository selection", func(c *api.GitHubChannelConfig) { c.Repositories = append(c.Repositories, "synthetic/other") }},
		{"unselected repository workflows", func(c *api.GitHubChannelConfig) {
			c.Trigger.Workflows["synthetic/other"] = []string{".github/workflows/check.yml"}
		}},
		{"legacy actions mixed", func(c *api.GitHubChannelConfig) { c.Events = []string{"opened"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := valid()
			tc.mutate(ch.Spec.GitHub)
			require.Error(t, (Kind{}).ValidateSpec(ch))
		})
	}
}
