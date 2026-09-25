//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/test/e2e"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// TestDependencyOAPInstallsDelegatesAndUninstalls proves a composed folder is
// the whole installation input: the shared graph workflow installs both
// classes, the real controllers run a delegated child using the rewritten
// physical class name, and graph uninstall removes both classes. There is no
// raw-manifest apply for either AgentClass in this test.
func TestDependencyOAPInstallsDelegatesAndUninstalls(t *testing.T) {
	fixtureDir := oaptest.WriteDependencyBundle(t)
	bundle, err := oap.FromFolder(fixtureDir)
	require.NoError(t, err, "load the composed dependency fixture")

	h := e2e.Start(t, e2e.Options{DefaultTimeout: 30 * time.Second})
	ctx := context.Background()

	// The fixture classes intentionally inherit the cluster default model.
	// Supply the e2e harness's test provider at that normal settings tier; do
	// not add a provider directly to the fixture AgentClasses.
	h.ApplyManifest(`
apiVersion: v1
kind: Secret
metadata:
  name: dependency-e2e-model-token
  namespace: default
type: Opaque
stringData:
  api-key: unused-by-scripted-provider
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ClusterAgentSettings
metadata:
  name: cluster
spec:
  modelCatalog:
    - name: scripted
      provider: test
      tokenRef:
        namespace: default
        name: dependency-e2e-model-token
        key: api-key
      default: true
`)

	workflow := &install.Workflow{
		Client: h.K8s,
		Options: install.InstallOpts{
			Namespace:  "default",
			SourceKind: "file",
			SourceRef:  fixtureDir,
		},
	}
	plan, err := workflow.Plan(ctx, bundle, install.GraphAnswers{})
	require.NoError(t, err, "plan the complete dependency graph")
	result, err := workflow.Execute(ctx, plan)
	require.NoError(t, err, "install the complete dependency graph")
	require.Len(t, result.Nodes, 2)

	var coordinator, translator spiceboxv1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: oaptest.DependencyRootName}, &coordinator))
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: oaptest.DependencyPhysicalChildName}, &translator))
	assert.Equal(t, []string{oaptest.DependencyPhysicalChildName}, coordinator.Spec.Subagents)
	assert.Equal(t, []string{"single_turn", "task"}, coordinator.Spec.SubagentModes[oaptest.DependencyPhysicalChildName])

	// A fake channel is only the conversation transport. The two AgentClasses
	// above came exclusively from the OAP workflow.
	h.ApplyManifest(`
apiVersion: v1
kind: Secret
metadata:
  name: dependency-e2e-channel-creds
  namespace: default
type: Opaque
stringData:
  placeholder: unused-by-fake-channel
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: Channel
metadata:
  name: dependency-e2e
  namespace: default
spec:
  kind: fake
  role: both
  agentClass: fixture-coordinator
  credentialsRef:
    secretName: dependency-e2e-channel-creds
  fake:
    echo: false
`)
	h.WaitForAgentClassValid(oaptest.DependencyPhysicalChildName, 30*time.Second)
	h.WaitForAgentClassValid(oaptest.DependencyRootName, 30*time.Second)

	const (
		userPrompt    = "Ask the private translator to render the fixture phrase."
		delegatedTask = "Render the fixture phrase."
		childResult   = "The private fixture translation is complete."
	)
	h.LLM.OnUserMessage(userPrompt).Reply(e2e.ToolUse("delegate", map[string]any{
		"agent": oaptest.DependencyPhysicalChildName,
		"task":  delegatedTask,
		"mode":  "task",
	}))
	h.LLM.OnUserMessage(delegatedTask).Reply(e2e.ToolUse("return_result", map[string]any{
		"result": childResult,
	}))
	h.LLM.OnToolResult("delegate", e2e.ResultContains(childResult)).Reply(
		e2e.RespondToUser("Coordinator received: " + childResult),
	)
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "translations delivered"}),
	)

	h.SendUserMessage(userPrompt)
	h.ExpectAgentReply(e2e.Contains("Coordinator received", childResult))
	e2e.Eventually(t, 30*time.Second, func() bool {
		var sessions spiceboxv1alpha1.AgentSessionList
		if listErr := h.K8s.List(ctx, &sessions, client.InNamespace("default")); listErr != nil {
			return false
		}
		for i := range sessions.Items {
			if sessions.Items[i].Spec.Parent != nil &&
				sessions.Items[i].Spec.Class == oaptest.DependencyPhysicalChildName {
				return true
			}
		}
		return false
	}, "a delegated child session runs the privately named child class")

	deleted, err := install.UninstallGraph(ctx, h.K8s, oaptest.DependencyRootName, "default")
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	for _, name := range []string{oaptest.DependencyRootName, oaptest.DependencyPhysicalChildName} {
		var class spiceboxv1alpha1.AgentClass
		getErr := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &class)
		assert.True(t, apierrors.IsNotFound(getErr), "%s must be removed by graph uninstall", name)
	}
}
