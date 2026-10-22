package channel_test

import (
	"context"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"testing"
)

func TestWorkflowReportInputValidWithoutConversationalOutput(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-creds", Namespace: "default"}, Data: map[string][]byte{"app-id": []byte("1"), "private-key": []byte("key"), "installation-id": []byte("2"), "webhook-secret": []byte("secret")}}
	adoptguard.WithAdoptedLabel(secret)
	ch := &api.Channel{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-ci", Namespace: "default"}, Spec: api.ChannelSpec{Kind: "github", Role: api.ChannelRoleInput, AgentClass: "agent-cls", AuthzSubject: "service:synthetic-ci", CredentialsRef: api.ChannelCredentialsRef{SecretName: "synthetic-creds"}, Owner: &api.ChannelOwnerPolicy{Ownerless: &api.ChannelOwnerlessSource{Permission: "github_repo:synthetic/widgets#pull_request_author"}}, GitHub: &api.GitHubChannelConfig{AppSlug: "synthetic", Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/test.yml"}}}}}}
	cli := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t, rbacv1.AddToScheme)).WithObjects(ch, secret, newValidAgentClass("agent-cls", nil)).WithStatusSubresource(&api.Channel{}).Build()
	_, err := newUnitReconciler(cli).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ch)})
	require.NoError(t, err)
	var got api.Channel
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(ch), &got))
	condition := meta.FindStatusCondition(got.Status.Conditions, api.ChannelConditionValid)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionTrue, condition.Status, condition.Message)
}

func TestWorkflowReportInputValidatesOptionalOutput(t *testing.T) {
	output := func(name, destination string) client.Object {
		return &api.Channel{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: api.ChannelSpec{Kind: "slack", Role: api.ChannelRoleOutput, AgentClass: "agent-cls", Slack: &api.SlackChannelConfig{OutputDefaults: &api.SlackOutputDefaults{ChannelID: destination}}}}
	}
	for _, tc := range []struct {
		name    string
		outputs []client.Object
		valid   bool
	}{
		{"configured", []client.Object{output("notifications", "C_ONE")}, true},
		{"ambiguous", []client.Object{output("one", "C_ONE"), output("two", "C_TWO")}, false},
		{"missing destination", []client.Object{output("notifications", "")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-creds", Namespace: "default"}, Data: map[string][]byte{"app-id": []byte("1"), "private-key": []byte("key"), "installation-id": []byte("2"), "webhook-secret": []byte("secret")}}
			adoptguard.WithAdoptedLabel(secret)
			ch := &api.Channel{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-ci", Namespace: "default"}, Spec: api.ChannelSpec{Kind: "github", Role: api.ChannelRoleInput, AgentClass: "agent-cls", AuthzSubject: "service:synthetic-ci", CredentialsRef: api.ChannelCredentialsRef{SecretName: "synthetic-creds"}, Owner: &api.ChannelOwnerPolicy{Ownerless: &api.ChannelOwnerlessSource{Permission: "group:ci-operators#member"}}, GitHub: &api.GitHubChannelConfig{AppSlug: "synthetic", Trigger: &api.GitHubTriggerConfig{Event: "workflow_run", Actions: []string{"completed"}, Readiness: "all_completed", Workflows: map[string][]string{"synthetic/widgets": {".github/workflows/test.yml"}}}}}}
			objects := append([]client.Object{ch, secret, newValidAgentClass("agent-cls", nil)}, tc.outputs...)
			cli := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t, rbacv1.AddToScheme)).WithObjects(objects...).WithStatusSubresource(&api.Channel{}).Build()
			_, err := newUnitReconciler(cli).Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ch)})
			require.NoError(t, err)
			var got api.Channel
			require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(ch), &got))
			condition := meta.FindStatusCondition(got.Status.Conditions, api.ChannelConditionValid)
			require.NotNil(t, condition)
			if tc.valid {
				require.Equal(t, metav1.ConditionTrue, condition.Status, condition.Message)
			} else {
				require.Equal(t, metav1.ConditionFalse, condition.Status)
				require.Equal(t, api.ReasonChannelOutputBindingUnresolvable, condition.Reason)
			}
		})
	}
}
