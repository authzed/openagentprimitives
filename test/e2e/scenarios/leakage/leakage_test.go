//go:build e2e

package leakage_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"strings"
	"testing"
	"time"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
)

// Real-runner integration tests for the information-leakage gate.
//
// These tests sit at the highest possible integration level the harness
// supports: real internal/cmd/runner-shaped Loop (via InProcessRunnerFactory), real
// SpiceDB (via testspicedb), real channelsd pipeline + outbound relay, real
// fake channel kind, embedded NATS. They are the regression bar for the
// five wiring bugs that bit us during manual testing plus the channelsd-side gap we
// fixed in the current branch (no handler for in.info_leakage_approval_request
// → approval DM never reached the approver).
//
// **STATUS: each scenario below is t.Skip-gated until two pieces of
// harness infrastructure land:**
//
//  1. **InProcessRunnerFactory.buildLoop must wire the leakage-gate fields**
//     (loop.LeakageConfig, LookupToolMapping, TaintMemoryAppend,
//     TaintMemoryList, AuditMemoryAppend, RequesterCanonicalID, SpiceDBCheck,
//     SpiceDBLookupSubjects, LeakageGrantWriter, ChannelKindImpl,
//     LeakageApprovalPublish, plus a subscribeLeakageApprovalApplied
//     goroutine). internal/cmd/runner/main.go:805-887 has the production wiring;
//     mirror it in the factory.
//  2. **Fake channel kind must implement an `info_leakage_approval`
//     sub-channel sender** (kind.SubChannelSender("info_leakage_approval", ...))
//     plus a way for tests to capture + click prompts of this kind. The
//     existing fake kind has a `permission_request` sender + the
//     ApprovalDriver pattern (pkg/channels/channelkinds/fake/driver.go); model
//     info_leakage_approval after it. Likely needs a new sub-channel name
//     and a new prompt-capture surface on Harness (ExpectLeakageApprovalPrompt).
//
// Once those two pieces land each scenario below should remove its Skip and
// run green. The tests are written as contracts: the assertions describe
// the invariants the system MUST hold, not just what we currently produce.

// TestLeakage_RealRunner_HappyPath: D1. The single test that, if it had
// existed during the original implementation, would have caught all five
// historical wiring bugs (meta-tool dispatch bypass, Slack
// SetAudienceResolver missing on the runner side, LLM-vs-bare-name
// toolResourceMap lookup, args envelope unwrapping, requester subject
// prefix). Boots the real-runner-shaped fixture with an audience fully
// permitted on the accessed resource, scripts a single tool call, and
// asserts the message publishes cleanly with taint recorded + positive-
// path audit emissions but no approval flow firing.
func TestLeakage_RealRunner_HappyPath(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any {
		return map[string]any{"id": "happy", "title": "Hello"}
	})
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any {
		return map[string]any{"uuid": "uuid-1", "title": "Hello"}
	})
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any {
		return map[string]any{"ok": true}
	})
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	// Bootstrap relationships MUST be applied before the gate runs;
	// otherwise SpiceDB has the schema but none of the viewer tuples.
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// Set the fake channel's audience to {alice} — the SpiceDBBootstrap
	// already seeds record:happy as viewable by alice@example.com and
	// bob@example.com, so audience ⊆ permitted: no leak.
	//
	// Audience subjects are bare canonical IDs (no "user:" prefix);
	// the gate's subtractSubjects compares against the bare IDs that
	// spicedb.LookupSubjects returns — see pkg/channels/channelkinds/slack/audience_resolver.go.
	scriptAudience(t, h, []string{e2e.CanonicalForFakeEmail("alice@example.com").String()})

	// LLM script: register the more-specific tool_result rules FIRST so
	// they match before the catch-all OnUserMessage rule on retries.
	// After respond_to_user delivers, the LLM is asked again — terminate
	// with agent_work_complete so the runner doesn't burn turns.
	h.LLM.OnToolResult("respond_to_user", func(_ any) bool { return true }).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
	)
	h.LLM.OnToolResult("leakage_get_record", func(_ any) bool { return true }).Reply(
		e2e.RespondToUser("Got: Hello"),
	)
	h.LLM.OnUserMessage("fetch happy").Reply(
		e2e.ToolUse("leakage_get_record", map[string]any{
			"operation_id": "op-1", "_reason": "fetch",
			"args": map[string]any{"id": "happy"},
		}),
	)

	// Subscribe to the approval-request OUT subject so we can assert no
	// envelope flies (no leak).
	ns := h.Namespace()
	approvalSub, err := h.NATS().SubscribeSync("ap.session." + ns + ".*.out.info_leakage_approval_request")
	require.NoError(t, err, "subscribe approval-request OUT")
	t.Cleanup(func() { _ = approvalSub.Unsubscribe() })

	h.SendUserMessage("fetch happy", e2e.AsUser("alice@example.com"))
	got := h.ExpectAgentReply(e2e.Contains("Got: Hello"))
	t.Logf("agent replied: %q", got.Text)

	// Resolve the spawned AgentSession to scope memory queries.
	sess := e2e.FindOnlyAgentSession(t, h, ns)

	// 1. Taint memory has exactly one record for record:happy.
	taintRecs := listTaintAfter(t, h, ns, sess.Name, 5*time.Second, 1)
	require.Len(t, taintRecs, 1, "expected one taint record; got %+v", taintRecs)
	assert.Equal(t, "record", taintRecs[0].ResourceType, "taint resourceType")
	assert.Equal(t, "happy", taintRecs[0].ResourceID, "taint resourceID")
	assert.Equal(t, "view", taintRecs[0].Permission, "taint permission")

	// 2. Audit memory has read_permitted (read-side hook fired + passed)
	//    and respond_no_leak (write-side gate fired + cleared).
	auditKinds := listAuditKindsEventually(t, h, ns, sess.Name, 5*time.Second,
		[]string{"read_permitted", "respond_no_leak"})
	t.Logf("audit kinds observed: %v", auditKinds)
	assert.Contains(t, auditKinds, "read_permitted", "audit must record read_permitted")
	assert.Contains(t, auditKinds, "respond_no_leak", "audit must record respond_no_leak")

	// 3. NO approval-request envelope was ever published — happy path
	//    means the gate cleared without engaging the approval flow.
	if msg, err := approvalSub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("did NOT expect an info_leakage_approval_request on the happy path; got subject=%q", msg.Subject)
	}
}

// TestLeakage_RealRunner_LeakThenApprove: D2. THE regression bar for
// the bug fixed in this branch (channelsd had no handler for
// in.info_leakage_approval_request, so the approval DM never reached
// the approver and the session hung). Real-runner version of E2:
// drives the full flow via SendUserMessage + LLM script rather than
// publishing a synthetic envelope.
//
// Fixture: audience={alice, bob}, SpiceDB permits only alice on
// record:leaky#view. Bob is the leak. Approver subject
// group:approvers#member resolves to approver@example.com via the
// bootstrap.
func TestLeakage_RealRunner_LeakThenApprove(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any {
		return map[string]any{"id": "leaky", "title": "Secret"}
	})
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "uuid-1"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// Audience = {alice, bob}. record:leaky permits only alice. Bob is the leak.
	scriptAudience(t, h, []string{
		e2e.CanonicalForFakeEmail("alice@example.com").String(),
		e2e.CanonicalForFakeEmail("bob@example.com").String(),
	})

	// The info-leakage approver model resolves eligible approvers as the
	// tainted DATA's owners (any one may vouch; session standing not
	// required — see authz.ResolveApprovers). Make alice own record:leaky so
	// she can approve sharing her record with the unauthorized audience
	// member (bob). An unowned record would fail closed ("no one has
	// standing").
	e2e.WriteRel(t, h, "record", "leaky", "owner", "user", e2e.CanonicalForFakeEmail("alice@example.com").String(), "")

	// LLM script — same shape as D1 but tool returns id=leaky.
	h.LLM.OnToolResult("respond_to_user", func(_ any) bool { return true }).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
	)
	h.LLM.OnToolResult("leakage_get_record", func(_ any) bool { return true }).Reply(
		e2e.RespondToUser("Title: Secret"),
	)
	h.LLM.OnUserMessage("fetch leaky").Reply(
		e2e.ToolUse("leakage_get_record", map[string]any{
			"operation_id": "op-1", "_reason": "fetch",
			"args": map[string]any{"id": "leaky"},
		}),
	)

	// Send the message, capture the leakage approval prompt, click Approve.
	h.SendUserMessage("fetch leaky", e2e.AsUser("alice@example.com"))
	approval := h.ExpectLeakageApprovalPrompt(e2e.ForLeakageResource("record:leaky"))
	require.NotNil(t, approval, "leakage approval prompt captured")

	// Assert: bob is NAMED as the leak target. Slice C2's generic renderer names
	// recipients in the request's "Would share with" render Field rather than as
	// typed LeakedTo identities, so the projection surfaces them via WouldShareWith.
	// The host stamps the bare canonical id (`user:` prefix stripped — see
	// infoLeakageWouldShareWithFields), which is what LookupSubjects resolves bob's
	// audience membership to. NOTE: the render shows this canonical as plain TEXT,
	// not a Slack @mention — a known generic-renderer delta; restoring the @mention
	// needs structured leaked-to in Details plus a renderer enhancement (tracked
	// follow-up). The test's intent (bob is the leak target) is preserved.
	require.Len(t, approval.Prompt().WouldShareWith, 1, "exactly bob is named as leaked-to")
	assert.Contains(t, approval.Prompt().WouldShareWith, e2e.CanonicalForFakeEmail("bob@example.com").String(),
		"the 'Would share with' render Field names bob's canonical")

	// Click Approve as alice — record:leaky's owner, i.e. the vouching data
	// owner the approval routed to.
	approval.Approve(e2e.AsUser("alice@example.com"))

	// After approve: respond_to_user delivers, agent_work_complete fires.
	got := h.ExpectAgentReply(e2e.Contains("Title: Secret"))
	t.Logf("agent replied: %q", got.Text)

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)

	// Audit memory has the full lifecycle: read_permitted (read-side
	// hook), leakage_detected (write-side gate found the leak), and
	// leakage_approved (decision recorded after the approver clicked).
	auditKinds := listAuditKindsEventually(t, h, ns, sess.Name, 10*time.Second,
		[]string{"read_permitted", "leakage_detected", "leakage_approved"})
	t.Logf("audit kinds observed: %v", auditKinds)
	assert.Contains(t, auditKinds, "read_permitted", "read-side hook")
	assert.Contains(t, auditKinds, "leakage_detected", "write-side detected leak")
	assert.Contains(t, auditKinds, "leakage_approved", "approver said yes")

	// SpiceDB now carries an infoleakage_grant relationship. Read
	// directly rather than CheckPermission since the grant's permission
	// expression involves a caveated relation we'd need to supply
	// context for. The existence of any infoleakage_grant tuple in this
	// session's namespace is sufficient evidence that the
	// LeakageGrantWriter ran.
	grantCount := countRelationships(t, h, "infoleakage_grant")
	assert.Greater(t, grantCount, 0, "expected at least one infoleakage_grant relationship after Approve")
}

// TestLeakage_RealRunner_LeakThenDeny: D3. Same fixture as D2 but the
// approver denies. The runner's gate returns an IsError to the LLM
// AND publishes the generic fallback notice on the channel (no silent
// errors). The original tainted response MUST NOT appear.
func TestLeakage_RealRunner_LeakThenDeny(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any {
		return map[string]any{"id": "leaky", "title": "Secret"}
	})
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "uuid-1"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	scriptAudience(t, h, []string{
		e2e.CanonicalForFakeEmail("alice@example.com").String(),
		e2e.CanonicalForFakeEmail("bob@example.com").String(),
	})

	// alice owns record:leaky → she is the vouching data owner and the real
	// approver (see LeakThenApprove).
	e2e.WriteRel(t, h, "record", "leaky", "owner", "user", e2e.CanonicalForFakeEmail("alice@example.com").String(), "")

	// On the deny path the gate returns IsError to the LLM; the LLM's
	// follow-up call (with the gate error as tool_result) should
	// terminate via agent_work_complete so the test cleans up.
	h.LLM.OnToolResult("respond_to_user", func(_ any) bool { return true }).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "gate denied"}),
	)
	h.LLM.OnToolResult("leakage_get_record", func(_ any) bool { return true }).Reply(
		e2e.RespondToUser("Title: Secret"),
	)
	h.LLM.OnUserMessage("fetch leaky").Reply(
		e2e.ToolUse("leakage_get_record", map[string]any{
			"operation_id": "op-1", "_reason": "fetch",
			"args": map[string]any{"id": "leaky"},
		}),
	)

	h.SendUserMessage("fetch leaky", e2e.AsUser("alice@example.com"))
	approval := h.ExpectLeakageApprovalPrompt(e2e.ForLeakageResource("record:leaky"))
	require.NotNil(t, approval)
	approval.Deny(e2e.AsUser("alice@example.com"))

	// On a denied SHARE the gate yields to the user with a "how to proceed"
	// notice (the deny→yield behavior, not the generic blocked notice), and the
	// original "Title: Secret" must NEVER appear in the channel.
	got := h.ExpectAgentReply(e2e.Contains("wasn't able to share"))
	t.Logf("agent replied (deny notice): %q", got.Text)
	assert.Contains(t, got.Text, "How would you like me to proceed",
		"deny path yields a 'how to proceed' notice to the user")
	assert.NotContains(t, got.Text, "Secret", "tainted content MUST NOT leak to the channel after deny")

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	auditKinds := listAuditKindsEventually(t, h, ns, sess.Name, 10*time.Second,
		[]string{"leakage_detected", "leakage_denied"})
	assert.Contains(t, auditKinds, "leakage_detected", "write-side detected leak")
	assert.Contains(t, auditKinds, "leakage_denied", "approver said no")

	// No infoleakage_grant tuple was written on Deny.
	assert.Equal(t, 0, countRelationships(t, h, "infoleakage_grant"),
		"deny path MUST NOT write a grant")
}

// TestLeakage_RealRunner_LoggingMode: D4. AgentClass override to
// informationLeakage.mode=logging. Audience > permitted: gate detects
// the leak but lets the message through; audit records the event.
// No approval envelope flies.
func TestLeakage_RealRunner_LoggingMode(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
		// Override the AgentClass's leakage mode to logging.
		ExtraManifests: []string{leakageModeOverride("logging")},
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any {
		return map[string]any{"id": "leaky", "title": "Secret"}
	})
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "uuid-1"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	scriptAudience(t, h, []string{
		e2e.CanonicalForFakeEmail("alice@example.com").String(),
		e2e.CanonicalForFakeEmail("bob@example.com").String(),
	})

	h.LLM.OnToolResult("respond_to_user", func(_ any) bool { return true }).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
	)
	h.LLM.OnToolResult("leakage_get_record", func(_ any) bool { return true }).Reply(
		e2e.RespondToUser("Title: Secret"),
	)
	h.LLM.OnUserMessage("fetch leaky").Reply(
		e2e.ToolUse("leakage_get_record", map[string]any{
			"operation_id": "op-1", "_reason": "fetch",
			"args": map[string]any{"id": "leaky"},
		}),
	)

	ns := h.Namespace()
	approvalSub, err := h.NATS().SubscribeSync("ap.session." + ns + ".*.out.info_leakage_approval_request")
	require.NoError(t, err, "subscribe approval-request OUT")
	t.Cleanup(func() { _ = approvalSub.Unsubscribe() })

	h.SendUserMessage("fetch leaky", e2e.AsUser("alice@example.com"))
	got := h.ExpectAgentReply(e2e.Contains("Title: Secret"))
	t.Logf("agent replied: %q", got.Text)

	sess := e2e.FindOnlyAgentSession(t, h, ns)
	auditKinds := listAuditKindsEventually(t, h, ns, sess.Name, 10*time.Second,
		[]string{"would_block_leakage"})
	assert.Contains(t, auditKinds, "would_block_leakage", "logging mode must record would_block_leakage")

	// No approval envelope in logging mode.
	if msg, err := approvalSub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("logging mode MUST NOT publish info_leakage_approval_request; got %q", msg.Subject)
	}
}

// TestLeakage_RealRunner_DisabledMode: D5. mode=disabled — gate is a
// complete no-op even when audience > permitted. No audit, no taint,
// no approval.
func TestLeakage_RealRunner_DisabledMode(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
		ExtraManifests: []string{leakageModeOverride("disabled")},
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any {
		return map[string]any{"id": "leaky", "title": "Secret"}
	})
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "uuid-1"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	scriptAudience(t, h, []string{
		e2e.CanonicalForFakeEmail("alice@example.com").String(),
		e2e.CanonicalForFakeEmail("bob@example.com").String(),
	})

	h.LLM.OnToolResult("respond_to_user", func(_ any) bool { return true }).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
	)
	h.LLM.OnToolResult("leakage_get_record", func(_ any) bool { return true }).Reply(
		e2e.RespondToUser("Title: Secret"),
	)
	h.LLM.OnUserMessage("fetch leaky").Reply(
		e2e.ToolUse("leakage_get_record", map[string]any{
			"operation_id": "op-1", "_reason": "fetch",
			"args": map[string]any{"id": "leaky"},
		}),
	)

	h.SendUserMessage("fetch leaky", e2e.AsUser("alice@example.com"))
	got := h.ExpectAgentReply(e2e.Contains("Title: Secret"))
	t.Logf("agent replied: %q", got.Text)

	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	// Disabled mode is a complete no-op: no taint, no audit.
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess.Name}
	tRecs, err := infoleakagetaint.List(memory.WithSystemApproval(context.Background(), "e2e-test"), h.MemStore(), scope)
	require.NoError(t, err, "taint list")
	assert.Empty(t, tRecs, "disabled mode MUST NOT record taint")
	aRecs, err := infoleakageaudit.List(memory.WithSystemApproval(context.Background(), "e2e-test"), h.MemStore(), scope)
	require.NoError(t, err, "audit list")
	// Disabled mode records no LEAKAGE audit. The SessionEnd cleanup hook now
	// writes a lifecycle "session_end" record into the same memory kind on every
	// terminal path (it is bookkeeping, not a leakage event); filter it out so
	// this assertion remains a precise claim about the leakage audit stream.
	var leakageRecs []infoleakageaudit.AuditRecord
	for _, r := range aRecs {
		if r.Kind == "session_end" {
			continue
		}
		leakageRecs = append(leakageRecs, r)
	}
	assert.Empty(t, leakageRecs, "disabled mode MUST NOT record leakage audit")
}

// TestLeakage_RealRunner_UnmappedTool_Enforcing: D6. The unmapped_tool
// declared in the fixture (deliberately NOT in toolResourceMap) triggers
// the fail-closed unmapped-tool path. The gate returns an error;
// respond_to_user reports the gate failure to the LLM, AND publishes a
// channel-side fallback notice (no-silent-errors).
func TestLeakage_RealRunner_UnmappedTool_Enforcing(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    "alice@example.com",
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any { return map[string]any{"id": "happy"} })
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "uuid-1"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any {
		return map[string]any{"ok": true}
	})
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)
	scriptAudience(t, h, []string{e2e.CanonicalForFakeEmail("alice@example.com").String()})

	// Script: LLM calls the unmapped tool. The leakage hook fires post-
	// execute, finds no toolResourceMap entry, returns an error. The
	// LLM gets the error back as a tool_result; on the next turn, just
	// terminate the session.
	h.LLM.OnToolResult("leakage_unmapped_tool", func(_ any) bool { return true }).Reply(
		e2e.ToolUse("agent_work_complete", map[string]any{"summary": "tool unmapped"}),
	)
	h.LLM.OnUserMessage("call unmapped").Reply(
		e2e.ToolUse("leakage_unmapped_tool", map[string]any{
			"operation_id": "op-1", "_reason": "test",
			"args": map[string]any{"id": "x"},
		}),
	)

	h.SendUserMessage("call unmapped", e2e.AsUser("alice@example.com"))

	// Wait for the session to terminate (Idle or Failed). Then check
	// the transcript for the unmapped-tool error.
	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	e2e.Eventually(t, 15*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseIdle ||
			s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed
	}, "session terminates")

	// The audit log records the misconfiguration via unmapped_tool
	// (logging mode) or surfaces it via the error path. In enforcing
	// mode the hook returns an error which surfaces to the LLM —
	// taint is NOT recorded for the unmapped tool.
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess.Name}
	tRecs, err := infoleakagetaint.List(memory.WithSystemApproval(context.Background(), "e2e-test"), h.MemStore(), scope)
	require.NoError(t, err, "taint list")
	for _, r := range tRecs {
		assert.NotEqual(t, "unmapped_tool", r.ResourceType,
			"unmapped tool must NOT have recorded taint")
	}
}

// leakageModeOverride returns a YAML patch manifest that overrides the
// agent-leakage-e2e AgentClass's informationLeakage.mode. Applied via
// ExtraManifests so per-test mode variants don't fork the base fixture.
func leakageModeOverride(mode string) string {
	return `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentClass
metadata:
  name: leakage-e2e
  namespace: default
spec:
  displayName: LeakageE2E
  description: "E2E fixture for the information-leakage gate."
  model:
    provider: test
    name: scripted
    apiKey:
      name: leakage-placeholder
      key: api-key
  systemPrompt:
    inline: "Test fixture."
  agentIdentity: leakage-identity
  mcpServers:
    - name: leakage
      ref: leakage-records
  authz:
    toolCalls:
      mode: disabled
      approver: "group:approvers#member"
    informationLeakage:
      mode: ` + mode + `
      approvalTTL: 1m
      approvalTimeout: 30s
      onUnsupportedChannel: blockBinding
      singleUserBypass: false
      loggingNoticeToRequester: false
  budget:
    maxTurns: 10
    maxTokens: 50000
    maxDuration: 5m
`
}

// The channelsd-isolation info_leakage round-trip (TestLeakage_ChannelsdRoundTrip)
// was removed in Slice C2: it injected a synthetic legacy
// KindInfoLeakageApprovalRequest to exercise the retired typed channelsd path.
// info_leakage now rides the generic Interaction model (buildLeakagePending
// publishes interaction_request; the pipe's HandleInteractionDecision +
// DecideResourceOwners + ApprovalDecisionHandler resolve it), and the
// TestLeakage_RealRunner_LeakThenApprove / LeakThenDeny scenarios above exercise
// that round-trip end-to-end through the flipped runner.

// countRelationships returns the number of relationships in SpiceDB
// for the given resource type. Used by tests to assert the runner's
// grant-writer (or any post-flow write) produced at least one tuple.
func countRelationships(t *testing.T, h *e2e.Harness, resourceType string) int {
	t.Helper()
	stream, err := h.SpiceDB.ReadRelationships(context.Background(), &spicedbv1.ReadRelationshipsRequest{
		RelationshipFilter: &spicedbv1.RelationshipFilter{ResourceType: resourceType},
	})
	require.NoError(t, err, "ReadRelationships %s", resourceType)
	n := 0
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
		n++
	}
	return n
}

// scriptAudience writes the audience the fake channel kind's
// AudienceResolver will return at gate time. Driver lookup mirrors the
// fake.Kind's resolver: one channel per namespace is the common case;
// we find that channel via the AgentClass's bound Channel CR.
func scriptAudience(t *testing.T, h *e2e.Harness, subjects []string) {
	t.Helper()
	ch := h.SingleChannel("scriptAudience")
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	require.NotNil(t, drv, "fake driver must exist for channel %s/%s", ch.Namespace, ch.Name)
	drv.SetAudience(subjects)
}

// listTaintAfter polls the in-process memory store for infoleakage_taint
// records scoped to the given session, returning once at least `want`
// records exist or the deadline elapses.
func listTaintAfter(t *testing.T, h *e2e.Harness, ns, name string, timeout time.Duration, want int) []infoleakagetaint.TaintRecord {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	deadline := time.Now().Add(timeout)
	var last []infoleakagetaint.TaintRecord
	for time.Now().Before(deadline) {
		recs, err := infoleakagetaint.List(memory.WithSystemApproval(context.Background(), "e2e-test"), h.MemStore(), scope)
		require.NoError(t, err, "taint list")
		last = recs
		if len(recs) >= want {
			return recs
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// listAuditKindsEventually polls the in-process memory store for
// infoleakage_audit records and returns the kinds set; waits until the
// requested kinds are all present (or deadline elapses).
func listAuditKindsEventually(t *testing.T, h *e2e.Harness, ns, name string, timeout time.Duration, wantKinds []string) []string {
	t.Helper()
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	deadline := time.Now().Add(timeout)
	var lastKinds []string
	for time.Now().Before(deadline) {
		recs, err := infoleakageaudit.List(memory.WithSystemApproval(context.Background(), "e2e-test"), h.MemStore(), scope)
		require.NoError(t, err, "audit list")
		lastKinds = lastKinds[:0]
		seen := map[string]bool{}
		for _, r := range recs {
			seen[r.Kind] = true
			lastKinds = append(lastKinds, r.Kind)
		}
		allPresent := true
		for _, k := range wantKinds {
			if !seen[k] {
				allPresent = false
				break
			}
		}
		if allPresent {
			return lastKinds
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastKinds
}

// Anchor unused imports for the still-Skipped scenarios so removing
// them later doesn't require a coordinated import-cleanup.
var _ = strings.Contains
