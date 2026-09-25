//go:build e2e

// Package agentui_test proves the update_view/read_view meta-tool
// capabilities (pkg/agent/tool/meta/capability/agentui_views.go) are wired
// through the REAL production/harness paths — internal/cmd/runner/main.go's
// uiViewRT construction and runner.AttachUIView call, and
// test/e2e/inprocess_runner_factory.go's identical wiring — not just the
// package-internal unit coverage in
// pkg/agent/tool/meta/capability/agentui_views_test.go. A capability gated
// correctly but never wired offers no evidence of working end-to-end; this
// is the belt on that unit coverage's suspenders.
package agentui_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// uiviewsCapabilityOverride re-applies the fixture's uiviews-agent
// AgentClass with spec.capabilities.update_view granted. Passed via
// e2e.Options.ExtraManifests, which applies AFTER AgentDir and updates the
// existing object in place — see channel_history_test.go's identical
// pattern (channelHistoryEnabledOverride).
const uiviewsCapabilityOverride = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: uiviews-agent
  namespace: default
spec:
  displayName: UIViewsBot
  description: "E2E fixture agent for the update_view/read_view capability gate. Do not deploy to a real cluster."
  model:
    provider: test
    name: scripted
    apiKey:
      name: uiviews-placeholder
      key: api-key
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      DO NOT deploy to a real cluster — your model provider is ` + "`test`" + `.
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
  agentUI:
    ref: uiviews-ui
  capabilities:
    update_view: {}
`

// markAgentUIValid stamps the fixture's AgentUI status Valid=True directly
// through the K8s client, bypassing pkg/controllers/agentui's reconciler —
// which this in-process e2e harness does not register (its manager wires
// only the AgentClass/AgentSession/… reconcilers actually exercised by
// existing scenarios; adding the AgentUI controller here would be growing
// the shared harness for this one task, which the brief for this task
// explicitly says not to do). The AgentClass controller (which IS
// registered) only READS AgentUI.Status.Conditions
// (pkg/controllers/agentclass/controller.go's validateAgentUI) and watches
// AgentUI for changes, so a directly-stamped status is indistinguishable
// to it from one the real reconciler would have produced, and
// WaitForAgentClassValid below observes the same watch-triggered
// re-reconcile a live AgentUI controller would have caused.
func markAgentUIValid(t *testing.T, h *e2e.Harness, ns, name string) {
	t.Helper()
	ctx := context.Background()
	var aui spiceboxv1alpha1.AgentUI
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &aui), "get fixture AgentUI")
	aui.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentUIConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonAgentUISpecOK,
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, h.K8s.Status().Update(ctx, &aui), "stamp fixture AgentUI Valid=True")
}

// TestUIViewCapabilities_NotOptedIn_ToolNotOffered proves update_view is
// never offered to the model when the AgentClass grants no capability for
// it, against the real capability.Assemble wiring internal/cmd/runner/main.go and
// the in-process factory both drive — not the package-internal fixture
// TestTheTwoGatesAreIndependent exercises.
func TestUIViewCapabilities_NotOptedIn_ToolNotOffered(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-ui-views-e2e"),
	})
	markAgentUIValid(t, h, "default", "uiviews-ui")

	h.WaitForAgentClassValid("uiviews-agent", 30*time.Second)

	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hi there"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage("hello")
	h.ExpectAgentReply(e2e.Contains("hi there"))
	h.AssertAllRulesConsumed()

	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "expected the scripted LLM to have observed at least one request")
	for _, req := range reqs {
		for _, tl := range req.Tools {
			assert.NotEqual(t, "update_view", tl.Name,
				"update_view must not be offered when spec.capabilities.update_view is absent")
		}
	}
}

// TestUIViewCapabilities_OptIn_ToolOffered is the positive row: granting
// spec.capabilities.update_view makes it appear in the model's tool
// catalog. Proves the capability's Offer actually contributes the tool
// through internal/cmd/runner/main.go/test/e2e/inprocess_runner_factory.go's real
// uiViewRT + runner.AttachUIView wiring, not just that the package-level
// unit tests believe it would.
func TestUIViewCapabilities_OptIn_ToolOffered(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-ui-views-e2e"),
		ExtraManifests: []string{uiviewsCapabilityOverride},
	})
	markAgentUIValid(t, h, "default", "uiviews-ui")

	h.WaitForAgentClassValid("uiviews-agent", 30*time.Second)

	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hi there"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage("hello")
	h.ExpectAgentReply(e2e.Contains("hi there"))
	h.AssertAllRulesConsumed()

	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "expected the scripted LLM to have observed at least one request")
	found := false
	for _, req := range reqs {
		for _, tl := range req.Tools {
			if tl.Name == "update_view" {
				found = true
			}
		}
	}
	assert.True(t, found, "update_view must be offered when spec.capabilities.update_view is granted")
}
