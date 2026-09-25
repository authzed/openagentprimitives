//go:build e2e

// test/e2e/reportsessioncost_test.go
package session_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"

	// Registers the session_cost SessionEnd hook factory (via its init) into the
	// runner's global hook registry for this e2e binary. internal/cmd/runner pulls this in
	// via its own blank import; the in-process harness has no such import path, so
	// without this the factory is absent and buildPipelineRegistry never builds
	// the cost hook (no stamp, no message).
	_ "github.com/authzed/openagentprimitives/pkg/agent/postsession/cost"
)

// Real-runner e2e coverage for the reportSessionCost feature end to end:
// ClusterAgentSettings.spec.defaults.reportSessionCost → EffectiveSettings →
// runner.Loop.ReportSessionCost → the session_cost SessionEnd pipeline hook
// (pkg/agent/postsession/cost) → status.estimatedCost + a channel Notice. Mirrors
// TestConversation_PingPong's fixture/script shape (centerdot-companies +
// ScriptedLLM + MCPStub) plus the AgentSession.Status polling pattern from
// leakage_test.go (findOnlyAgentSession / eventually).
//
// Harness wiring this scenario required (added alongside these tests, all
// test-infrastructure-only — no production code changed):
//   - InProcessRunnerFactory.buildLoop now sets Loop.ReportSessionCost from
//     sess.Status.EffectiveSettings.ReportSessionCost and wires Loop.Notify
//     to publish a KindNotification envelope (mirroring internal/cmd/runner/main.go's
//     buildLoopNotify) — previously neither was wired, so the hook was never
//     registered in-process and Notify(...) was silently a no-op.
//   - ScriptedLLM.Send now attaches a small fixed non-zero Usage to every
//     response (previously always zero), so cumulative-usage × Pricing has
//     something to multiply — without it amountMicroUSD would always be 0.
//   - The fake channel kind's Sender now handles KindNotification (captured
//     on Driver.notifications, drained via the new Driver.Notifications()),
//     and Harness.ExpectNotification mirrors ExpectAgentReply for it.
//
// Runner semantics note: for a CHANNEL-ATTACHED session (every e2e
// conversational scenario binds a fake Channel), the runner never reaches
// SessionEndInfo.Reason=="completed" — pkg/agent/runner/loop.go routes
// agent_work_complete (and the other clean-terminal paths) to "idle" for any
// ChannelAttached session; "completed" is written only on the two
// non-ChannelAttached (kubectl-driven) paths. The cost hook's
// message-eligibility check (pkg/agent/postsession/cost.Eval) treats "completed"
// and "failed" identically — both take the "true terminal, not idle" branch
// that emits the Notice — so the "message delivered" scenario below drives a
// budget-exceeded FAILURE (deterministic; needs no ScriptedLLM changes)
// rather than an unreachable "completed" state. This exercises the exact
// same hook branch a "completed" reason would.

// TestReportSessionCost_TerminalFailure_DeliversMessageAndStampsCost: with
// reportSessionCost on (the fixture applies no ClusterAgentSettings, so it
// defaults to true) and a budget of maxTurns=1, the session fails on the
// second loop iteration (post-dispatch budget check) after respond_to_user's
// single reply — a true terminal, non-idle reason. Asserts the cost Notice
// is delivered AND status.estimatedCost is stamped with known pricing and a
// positive amount.
func TestReportSessionCost_TerminalFailure_DeliversMessageAndStampsCost(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		DefaultTimeout: 30 * time.Second,
		ExtraManifests: []string{centerdotMaxTurnsOneOverride()},
	})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Only ONE Send call happens before the budget trips: after
	// respond_to_user dispatches (non-terminal), the loop's post-dispatch
	// budget check (turnCount=1 >= maxTurns=1) fails the session before any
	// second LLM call is made. .Repeating guards against the same
	// cold-start double-ask race TestConversation_PingPong documents.
	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong")).Repeating()

	h.SendUserMessage("ping")
	h.ExpectAgentReply(e2e.Contains("pong"))

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	e2e.Eventually(t, 30*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed
	}, "session reaches Failed (budget exceeded)")

	msg := h.ExpectNotification(e2e.Contains("~$"))
	t.Logf("cost notice: %q", msg.Text)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: sess.Name}, &got), "get AgentSession")
	require.NotNil(t, got.Status.EstimatedCost, "status.estimatedCost must be stamped")
	assert.True(t, got.Status.EstimatedCost.PricingKnown, "ScriptedLLM.Pricing reports a known price for every model")
	assert.Greater(t, got.Status.EstimatedCost.AmountMicroUSD, int64(0),
		"synthetic non-zero usage x known pricing must be > 0")
}

// TestReportSessionCost_Idle_StampsCostWithoutMessage: reportSessionCost on
// (default), the session goes through respond_to_user then
// agent_work_complete — a channel-attached agent_work_complete always
// resolves to Idle, never "completed". Asserts NO cost notice is delivered
// (idle is explicitly excluded by pkg/agent/postsession/cost.Eval) but
// status.estimatedCost is still stamped, since the hook stamps on every
// terminal path regardless of message-eligibility.
func TestReportSessionCost_Idle_StampsCostWithoutMessage(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		DefaultTimeout: 30 * time.Second,
	})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
	)

	h.SendUserMessage("ping")
	h.ExpectAgentReply(e2e.Contains("pong"))

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	e2e.Eventually(t, 30*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle
	}, "session reaches Idle (agent_work_complete)")

	// Grace window for the SessionEnd hook to run and (if it were ever
	// going to) publish a notice, then assert none of the notifications
	// observed carry a cost figure. We can't prove a negative instantly;
	// this mirrors the leakage_test.go NextMsg-with-timeout precedent for
	// "assert this envelope never arrives".
	time.Sleep(500 * time.Millisecond)
	ch := h.SingleChannel("TestReportSessionCost_Idle_StampsCostWithoutMessage")
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv, "fake driver must be registered")
	for _, n := range drv.Notifications() {
		assert.NotContains(t, n.Text, "$", "idle must never emit the cost report notice")
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: sess.Name}, &got), "get AgentSession")
	require.NotNil(t, got.Status.EstimatedCost, "status.estimatedCost must still be stamped on the idle path")
	assert.True(t, got.Status.EstimatedCost.PricingKnown)
	assert.Greater(t, got.Status.EstimatedCost.AmountMicroUSD, int64(0))
}

// TestReportSessionCost_Off_NoMessageNoCostStamp: a ClusterAgentSettings
// singleton with spec.defaults.reportSessionCost=false resolves into
// EffectiveSettings.ReportSessionCost=false, so the session_cost hook's
// factory (pkg/agent/postsession/cost/register.go) never builds the hook at all —
// asserts neither a cost notice nor a status.estimatedCost stamp appears.
func TestReportSessionCost_Off_NoMessageNoCostStamp(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		DefaultTimeout: 30 * time.Second,
		ExtraManifests: []string{reportSessionCostOffOverride()},
	})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
	)

	h.SendUserMessage("ping")
	h.ExpectAgentReply(e2e.Contains("pong"))

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	e2e.Eventually(t, 30*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle
	}, "session reaches Idle (agent_work_complete)")

	time.Sleep(500 * time.Millisecond)
	ch := h.SingleChannel("TestReportSessionCost_Off_NoMessageNoCostStamp")
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv, "fake driver must be registered")
	for _, n := range drv.Notifications() {
		assert.NotContains(t, n.Text, "$", "reportSessionCost=false must never emit a cost notice")
	}

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: sess.Name}, &got), "get AgentSession")
	assert.Nil(t, got.Status.EstimatedCost, "reportSessionCost=false must never stamp status.estimatedCost")
}

// centerdotMaxTurnsOneOverride replaces the centerdot-companies AgentClass
// (applied by the agent-centerdot-companies fixture) with an identical spec
// except budget.maxTurns: 1, so a single respond_to_user round-trip is
// enough to exceed the turn budget and fail the session — the deterministic
// stand-in for an unreachable "completed" reason (see the file doc comment).
// ApplyManifest does a full typed Update, so this must replicate the whole
// spec, not just the budget field.
func centerdotMaxTurnsOneOverride() string {
	return `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: centerdot-companies
  namespace: default
spec:
  displayName: CenterdotBot
  description: "E2E fixture agent. Do not deploy to a real cluster."
  model:
    provider: test
    name: scripted
    apiKey:
      name: centerdot-placeholder
      key: api-key
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      DO NOT deploy to a real cluster — your model provider is ` + "`test`" + `
      and the production operator will reject this AgentClass.
  agentIdentity: centerdot-identity
  mcpServers:
    - name: centerdot
      ref: centerdot-companies
  authz:
    slots:
      - resourceType: crm_company
        description: "A CRM company record"
        permission: contact_access
  budget:
    maxTurns: 1
    maxTokens: 50000
    maxDuration: 5m
`
}

// reportSessionCostOffOverride is a fresh ClusterAgentSettings singleton
// (no prior CR exists in a bare envtest boot) turning reportSessionCost off
// at the cluster tier. Applied via ExtraManifests before any AgentSession is
// created, so settingswiring.ResolveForSession picks it up at session
// creation time.
func reportSessionCostOffOverride() string {
	return `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ClusterAgentSettings
metadata:
  name: cluster
spec:
  defaults:
    reportSessionCost: false
`
}

// TestReportSessionCost_CatalogPrice_OverridesBuiltInTable proves the runner's
// cost estimate is catalog-authoritative end to end: a ClusterAgentSettings
// model-catalog entry for the fixture's "scripted" model carries an explicit
// price (10 in / 20 out per MTok) distinct from ScriptedLLM's own built-in
// Pricing() table (1 in / 1 out per MTok — see scripted_llm.go). The
// AgentClass override resolves its model via model.fromCatalog (no inline
// provider/name/apiKey), which is what routes settingswiring.ResolveForSession
// through resolveModel's CATALOG branch (pkg/platform/settings/resolve.go) — an inline
// apiKey would instead take the bring-your-own branch, which never carries a
// catalog price regardless of what the catalog says (see
// pkg/platform/settings/modelprice_test.go's "BYO override path is unpriced" case).
//
// That resolved catalog price lands on
// AgentSession.status.effectiveSettings.modelInputPerMTok/modelOutputPerMTok
// (runner.Loop.ModelInputPerMTok/ModelOutputPerMTok), and
// pkg/agent/postsession/cost/register.go's catalogFirstPricing prefers it over
// Provider.Pricing() (ScriptedLLM's built-in table) whenever it is non-zero.
// Reuses the exact terminal-failure shape (budget.maxTurns: 1) as
// TestReportSessionCost_TerminalFailure_DeliversMessageAndStampsCost — see
// this file's doc comment for why a channel-attached session can only reach
// "failed", never "completed", and why the cost hook treats them identically.
func TestReportSessionCost_CatalogPrice_OverridesBuiltInTable(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-centerdot-companies"),
		DefaultTimeout: 30 * time.Second,
		ExtraManifests: []string{
			catalogPricedModelSettings(),
			centerdotCatalogPricedMaxTurnsOneOverride(),
		},
	})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Only ONE Send call happens before the budget trips (see the analogous
	// comment on TestReportSessionCost_TerminalFailure_DeliversMessageAndStampsCost).
	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong")).Repeating()

	h.SendUserMessage("ping")
	h.ExpectAgentReply(e2e.Contains("pong"))

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	e2e.Eventually(t, 30*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed
	}, "session reaches Failed (budget exceeded)")

	msg := h.ExpectNotification(e2e.Contains("~$"))
	t.Logf("cost notice: %q", msg.Text)

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: sess.Name}, &got), "get AgentSession")
	require.NotNil(t, got.Status.EstimatedCost, "status.estimatedCost must be stamped")
	assert.True(t, got.Status.EstimatedCost.PricingKnown, "the catalog price makes this model's pricing known")

	// ScriptedLLM's synthetic usage (100 input / 50 output tokens, no cache —
	// see syntheticUsage in scripted_llm.go) x the CATALOG price (10 in / 20
	// out per MTok) = 100*10 + 50*20 = 2000 microUSD. Had the hook instead
	// fallen back to ScriptedLLM.Pricing's built-in table (1 in / 1 out per
	// MTok) the amount would be 100*1 + 50*1 = 150 microUSD. Asserting the
	// exact catalog-derived figure — and that it is NOT the built-in-table
	// figure — is what proves the catalog won, not just "some positive cost".
	const wantCatalogMicroUSD = int64(2000)
	const builtInTableMicroUSD = int64(150)
	assert.Equal(t, wantCatalogMicroUSD, got.Status.EstimatedCost.AmountMicroUSD,
		"cost must be computed from the CATALOG price (10 in / 20 out per MTok), not ScriptedLLM's built-in table")
	assert.NotEqual(t, builtInTableMicroUSD, got.Status.EstimatedCost.AmountMicroUSD,
		"must not equal what ScriptedLLM's built-in Pricing() table would have produced")
}

// catalogPricedModelSettings is a fresh ClusterAgentSettings singleton (no
// prior CR exists in a bare envtest boot) carrying a modelCatalog entry for
// the fixture's "scripted" model with an explicit price (10 in / 20 out per
// MTok) deliberately distinct from ScriptedLLM's built-in Pricing() table (1
// in / 1 out). tokenRef points at the centerdot-placeholder Secret
// 00-secret.yaml already applies (namespace default, key api-key) — reused
// rather than minting a second placeholder Secret, since the e2e harness's
// ScriptedLLM ignores the token value entirely; the AgentSession controller's
// materializeCatalogToken (pkg/controllers/agentsession/controller.go) still
// does a live Secret Get before a fresh session's runner is created, so the
// Secret must exist for real.
func catalogPricedModelSettings() string {
	return `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: ClusterAgentSettings
metadata:
  name: cluster
spec:
  modelCatalog:
    - name: scripted
      provider: test
      tokenRef:
        namespace: default
        name: centerdot-placeholder
        key: api-key
      inputPerMTok: 10
      outputPerMTok: 20
      default: true
`
}

// centerdotCatalogPricedMaxTurnsOneOverride is centerdotMaxTurnsOneOverride's
// spec with model switched from an inline provider/name/apiKey to
// fromCatalog: scripted, so settingswiring.ResolveForSession's catalog branch
// (pkg/platform/settings/resolve.go resolveModel) applies instead of the legacy
// branch — an inline apiKey would instead take the bring-your-own branch,
// which never carries a catalog price. budget.maxTurns: 1 reproduces the same
// deterministic terminal-failure shape as centerdotMaxTurnsOneOverride (see
// this file's doc comment on why "completed" is unreachable for a
// channel-attached session). ApplyManifest does a full typed Update, so this
// must replicate the whole spec, not just the model field.
func centerdotCatalogPricedMaxTurnsOneOverride() string {
	return `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: centerdot-companies
  namespace: default
spec:
  displayName: CenterdotBot
  description: "E2E fixture agent. Do not deploy to a real cluster."
  model:
    fromCatalog: scripted
  systemPrompt:
    inline: |
      You are a fixture agent for the agentprimitives e2e harness.
      DO NOT deploy to a real cluster — your model provider is ` + "`test`" + `
      and the production operator will reject this AgentClass.
  agentIdentity: centerdot-identity
  mcpServers:
    - name: centerdot
      ref: centerdot-companies
  authz:
    slots:
      - resourceType: crm_company
        description: "A CRM company record"
        permission: contact_access
  budget:
    maxTurns: 1
    maxTokens: 50000
    maxDuration: 5m
`
}
