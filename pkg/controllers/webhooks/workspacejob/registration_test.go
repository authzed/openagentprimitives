package workspacejob

import (
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func TestShippedWorkspaceJobAdmission(t *testing.T) {
	docs, err := manifests.Split(manifests.Install)
	require.NoError(t, err)
	var hook *admissionregistrationv1.ValidatingWebhook
	for _, doc := range docs {
		if doc.GetKind() != "ValidatingWebhookConfiguration" {
			continue
		}
		var config admissionregistrationv1.ValidatingWebhookConfiguration
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(doc.Object, &config))
		for i := range config.Webhooks {
			if config.Webhooks[i].Name == "workspacejob.agentprimitives.authzed.com" {
				hook = &config.Webhooks[i]
			}
		}
	}
	require.NotNil(t, hook)
	require.NotNil(t, hook.FailurePolicy)
	require.Equal(t, admissionregistrationv1.Fail, *hook.FailurePolicy)
	require.NotNil(t, hook.ClientConfig.Service)
	require.NotNil(t, hook.ClientConfig.Service.Path)
	require.Equal(t, Path, *hook.ClientConfig.Service.Path)
	require.Len(t, hook.MatchConditions, 1)
	require.Equal(t, "request.userInfo.username.startsWith('system:serviceaccount:') && request.userInfo.username.endsWith('-runner-sa')", hook.MatchConditions[0].Expression)
	require.Len(t, hook.Rules, 1)
	require.Equal(t, []string{"batch"}, hook.Rules[0].APIGroups)
	require.Equal(t, []string{"jobs"}, hook.Rules[0].Resources)
	require.Equal(t, []admissionregistrationv1.OperationType{admissionregistrationv1.Create}, hook.Rules[0].Operations)
}
