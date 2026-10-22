package pipeline

import (
	"context"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestWorkflowReportSessionKeepsOnlyVerifiedInputBinding(t *testing.T) {
	ch := newKindAuthzSubjectChannel("synthetic-ci", "github", "service:synthetic-ci")
	ch.Spec.Role = api.ChannelRoleInput
	ch.Spec.GitHub = &api.GitHubChannelConfig{Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/test.yml"}}}}
	p, _, _, _, cli := newPipeline(t, ch)
	decision, err := p.Deliver(context.Background(), channelkinds.InboundEvent{Channel: ch, ChannelKey: "pr:synthetic/widgets#7", MessageText: "CI settled", AuthzSubject: "service:synthetic-ci"})
	require.NoError(t, err)
	require.Equal(t, channelkinds.OutcomeRouted, decision.Outcome)
	var sessions api.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	require.Nil(t, sessions.Items[0].Spec.OutputChannel)
	require.NotNil(t, sessions.Items[0].Spec.InputChannel)
	require.Equal(t, "synthetic-ci", sessions.Items[0].Spec.InputChannel.Name)
}

func TestWorkflowReportSessionBindsOptionalConversationalOutput(t *testing.T) {
	ch := newKindAuthzSubjectChannel("synthetic-ci", "github", "service:synthetic-ci")
	ch.Spec.Role = api.ChannelRoleInput
	ch.Spec.GitHub = &api.GitHubChannelConfig{Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/test.yml"}}}}
	p, _, _, _, cli := newPipeline(t, ch, newSlackOutputChannel("ci-notifications", "C_COVERAGE"))
	decision, err := p.Deliver(context.Background(), channelkinds.InboundEvent{Channel: ch, ChannelKey: "pr:synthetic/widgets#7", MessageText: "CI settled", AuthzSubject: "service:synthetic-ci"})
	require.NoError(t, err)
	require.Equal(t, channelkinds.OutcomeRouted, decision.Outcome)
	var sessions api.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	require.NotNil(t, sessions.Items[0].Spec.InputChannel)
	require.Equal(t, "synthetic-ci", sessions.Items[0].Spec.InputChannel.Name)
	require.NotNil(t, sessions.Items[0].Spec.OutputChannel)
	require.Equal(t, "ci-notifications", sessions.Items[0].Spec.OutputChannel.Name)
	require.Equal(t, "slack", sessions.Items[0].Spec.OutputChannel.Kind)
}

func TestWorkflowReportSessionRefusesAmbiguousOutput(t *testing.T) {
	ch := newKindAuthzSubjectChannel("synthetic-ci", "github", "service:synthetic-ci")
	ch.Spec.Role = api.ChannelRoleInput
	ch.Spec.GitHub = &api.GitHubChannelConfig{Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/test.yml"}}}}
	p, _, _, _, cli := newPipeline(t, ch, newSlackOutputChannel("ci-one", "C_ONE"), newSlackOutputChannel("ci-two", "C_TWO"))
	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{Channel: ch, ChannelKey: "pr:synthetic/widgets#7", MessageText: "CI settled", AuthzSubject: "service:synthetic-ci"})
	require.ErrorContains(t, err, "multiple role=output")
	var sessions api.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &sessions))
	require.Empty(t, sessions.Items)
}
