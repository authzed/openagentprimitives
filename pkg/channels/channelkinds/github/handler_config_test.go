package github

import (
	"encoding/json"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/stretchr/testify/require"
)

func TestHandlerConfigRejectsUnsupportedArguments(t *testing.T) {
	for _, raw := range []string{`{"unknown":true}`, `{"trigger":{"event":"workflow_run","actions":["requested"],"readiness":"all_completed"}}`, `{"trigger":{"event":"workflow_run","readiness":"anything"}}`, `{"trigger":{"event":"workflow_run","typo":true}}`} {
		require.Error(t, (Kind{}).ValidateHandlerConfig(json.RawMessage(raw)))
	}
	require.NoError(t, (Kind{}).ValidateHandlerConfig(json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)))
}

func TestWorkflowInstallAnswersRequiredAndRepositoryScoped(t *testing.T) {
	in := channelkinds.WizardInput{Config: json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)}
	_, err := effectiveHandlerConfig(in, nil)
	require.Error(t, err)
	cfg, err := effectiveHandlerConfig(in, map[string]string{"workflows": `{"synthetic/widgets":[".github/workflows/unit.yaml",".github/workflows/quality.yaml"]}`})
	require.NoError(t, err)
	require.Len(t, cfg.Trigger.Workflows["synthetic/widgets"], 2)
	require.Equal(t, []string{"synthetic/widgets"}, cfg.Repositories)
}

func TestWorkflowReconfigurationPreservesSelection(t *testing.T) {
	in := channelkinds.WizardInput{Config: json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed","workflows":{"synthetic/widgets":[".github/workflows/compile.yml"]}}}`)}
	cfg, err := effectiveHandlerConfig(in, nil)
	require.NoError(t, err)
	require.Equal(t, []string{".github/workflows/compile.yml"}, cfg.Trigger.Workflows["synthetic/widgets"])
	cfg, err = effectiveHandlerConfig(in, map[string]string{"workflows": `{"synthetic/other":[".github/workflows/verify.yml"]}`})
	require.NoError(t, err)
	require.NotContains(t, cfg.Trigger.Workflows, "synthetic/widgets")
}

func TestWorkflowDeclarationMatchesInstalledAnswers(t *testing.T) {
	raw := json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)
	cfg, err := effectiveHandlerConfig(channelkinds.WizardInput{Config: raw}, map[string]string{"workflows": `{"synthetic/widgets":[".github/workflows/check.yml"]}`})
	require.NoError(t, err)
	channel := &api.Channel{Spec: api.ChannelSpec{GitHub: cfg}}
	require.NoError(t, (Kind{}).HandlerConfigMatches(raw, channel))
	channel.Spec.GitHub.Trigger = nil
	require.Error(t, (Kind{}).HandlerConfigMatches(raw, channel))
}

func TestWorkflowQuestionAndLegacyDefaults(t *testing.T) {
	qs, err := workflowQuestions(channelkinds.WizardInput{Config: json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)})
	require.NoError(t, err)
	require.Len(t, qs, 2)
	require.Equal(t, "workflows", qs[0].Name)
	require.Nil(t, qs[0].Default)
	cfg, err := effectiveHandlerConfig(channelkinds.WizardInput{}, nil)
	require.NoError(t, err)
	require.Nil(t, cfg.Trigger)
}

func TestWorkflowInstallDeclaresSessionOwner(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "default", Config: json.RawMessage(`{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`)}
	answers := demoAnswers()
	answers["workflows"] = `{"synthetic/widgets":[".github/workflows/check.yml"]}`
	_, err := (&wizard{}).Result(in, answers)
	require.ErrorContains(t, err, "session-owner-permission")
	answers["session-owner-permission"] = "group:ci-operators#member"
	out, err := (&wizard{}).Result(in, answers)
	require.NoError(t, err)
	require.NotNil(t, out.ChannelManifest.Spec.Owner)
	require.Equal(t, "group:ci-operators#member", out.ChannelManifest.Spec.Owner.Ownerless.Permission)
	answers["session-owner-permission"] = "malformed"
	_, err = (&wizard{}).Result(in, answers)
	require.ErrorContains(t, err, "session-owner-permission")
}
