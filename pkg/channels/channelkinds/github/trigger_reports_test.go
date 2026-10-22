package github

import (
	"encoding/json"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTriggerReportPublicationIsWorkflowOptIn(t *testing.T) {
	k := Kind{}
	require.False(t, k.PublishesTriggerReports(nil))
	require.False(t, k.PublishesTriggerReports(&api.Channel{Spec: api.ChannelSpec{GitHub: &api.GitHubChannelConfig{}}}))
	ch := &api.Channel{Spec: api.ChannelSpec{GitHub: &api.GitHubChannelConfig{Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/test.yml"}}}}}}
	require.True(t, k.PublishesTriggerReports(ch))
	require.True(t, k.ConfigPublishesTriggerReports(json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)))
	require.False(t, k.ConfigPublishesTriggerReports(json.RawMessage(`{"trigger":{"event":"workflow_run","readiness":"unsupported"}}`)))
}
