// pkg/agent/runner/tool_auth_mode_test.go
//
// Tests for the three-mode spec.authz.toolCalls.mode dispatch behavior.
// Mirrors the layout of authz_test.go / dispatch_approval_test.go and
// reuses their helpers (countingTool, makeAuthzLoop, fakeAuthzClient,
// scriptedSpiceDB).
package runner_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/toolcheck"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// externalToolScript drives the LLM to call an external tool exactly
// once, then close out with agent_work_complete.
func externalToolScript(toolName, toolUseID, args string) []llmfake.Step {
	return []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "text", Text: "I need to perform an external action."},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID:    toolUseID,
					Name:  toolName,
					Input: json.RawMessage(args),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		workCompleteResp(),
	}
}

// TestDispatch_DisabledMode_SkipsApproval verifies that an external tool
// (which would always require approval in enforcing mode) is dispatched
// to Execute without any approval flow when ToolAuthMode is "disabled".
// Asserts:
//   - tool.Execute fires
//   - no ApprovalPublish call was made
//   - DisabledNotify fires exactly once
func TestDispatch_DisabledMode_SkipsApproval(t *testing.T) {
	externalTool := &countingTool{
		name: "send_email",
		perm: authz.Permission{
			StateImpact: authz.External,
			Check: &authz.PermissionCheck{
				ResourceType:       "mailbox",
				ResourceIDTemplate: "{to}",
				Permission:         "send",
			},
		},
	}
	tools := append(meta.Load(), externalTool)
	script := externalToolScript("send_email", "tu_ext", `{"to":"someone@example.com","body":"hi"}`)

	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "alice@example.com",
	}
	l, _ := makeAuthzLoop(t, tools, script, annotations)

	// Wire SpiceDB even though disabled mode should never call it.
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	// Set mode and wire Approval + the interaction publisher to detect erroneous
	// use (Slice C2: tool_call publishes via InteractionRequestPublish).
	l.ToolAuthMode = runner.ToolAuthModeDisabled
	l.Approval = approval.New()
	var publishedCount atomic.Int32
	l.InteractionRequestPublish = func(_ context.Context, _ string, _ string, _ channelevents.Envelope) error {
		publishedCount.Add(1)
		return nil
	}
	var disabledNotifyCount atomic.Int32
	l.DisabledNotify = func(_ context.Context) {
		disabledNotifyCount.Add(1)
	}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 1, externalTool.executes, "Execute should fire once in disabled mode")
	assert.Equal(t, int32(0), publishedCount.Load(), "disabled mode must NOT publish approval envelopes")
	assert.Equal(t, 0, cli.callsN, "disabled mode must NOT call SpiceDB")
	assert.Equal(t, int32(1), disabledNotifyCount.Load(), "DisabledNotify should fire exactly once per session")
}

// TestDispatch_DisabledMode_NotifyOnceAcrossDispatches verifies the
// one-shot semantics of DisabledNotify: a session that runs two tool
// calls produces exactly one notification.
func TestDispatch_DisabledMode_NotifyOnceAcrossDispatches(t *testing.T) {
	tool1 := &countingTool{name: "tool_one", perm: authz.Permission{StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{ResourceType: "r", ResourceIDTemplate: "x", Permission: "read"}}}
	tool2 := &countingTool{name: "tool_two", perm: authz.Permission{StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{ResourceType: "r", ResourceIDTemplate: "x", Permission: "write"}}}

	// Two scripted turns, each with a different tool_use, then a final
	// agent_work_complete.
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu1", Name: "tool_one", Input: json.RawMessage(`{}`),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5},
		}},
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu2", Name: "tool_two", Input: json.RawMessage(`{}`),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5},
		}},
		workCompleteResp(),
	}
	tools := append(meta.Load(), tool1, tool2)
	l, _ := makeAuthzLoop(t, tools, script, nil)
	l.ToolAuthMode = runner.ToolAuthModeDisabled
	var n atomic.Int32
	l.DisabledNotify = func(_ context.Context) { n.Add(1) }

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, int32(1), n.Load(), "DisabledNotify should be one-shot per session")
	assert.Equal(t, 1, tool1.executes, "tool1 should have executed")
	assert.Equal(t, 1, tool2.executes, "tool2 should have executed")
}

// TestDispatch_PermissiveMode_DenyProceeds verifies that in permissive
// mode, a denied readonly Check is logged as "would deny" but the tool
// still runs (no approval pause, no IsError).
func TestDispatch_PermissiveMode_DenyProceeds(t *testing.T) {
	readTool := &countingTool{
		name: "read_repo",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}
	tools := append(meta.Load(), readTool)
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu_read", Name: "read_repo", Input: json.RawMessage(`{"repo":"spicedb"}`),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		workCompleteResp(),
	}

	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "alice@example.com",
	}
	l, _ := makeAuthzLoop(t, tools, script, annotations)
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	l.ToolAuthMode = runner.ToolAuthModePermissive
	l.Approval = approval.New()
	var publishedCount atomic.Int32
	l.InteractionRequestPublish = func(_ context.Context, _ string, _ string, _ channelevents.Envelope) error {
		publishedCount.Add(1)
		return nil
	}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 1, readTool.executes, "permissive: readonly tool should run despite deny")
	assert.Equal(t, int32(0), publishedCount.Load(), "permissive: readonly deny must NOT publish approval")
	assert.Equal(t, 1, cli.callsN, "permissive: SpiceDB should still be Check'd once")
}

// TestDispatch_PermissiveMode_DenyProceeds_NoApprovalWired exercises the
// branch where Approval is nil (kubectl-driven sessions, tests). The
// denied Check still proceeds to Execute under permissive.
func TestDispatch_PermissiveMode_DenyProceeds_NoApprovalWired(t *testing.T) {
	readTool := &countingTool{
		name: "read_repo",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}
	tools := append(meta.Load(), readTool)
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu_read", Name: "read_repo", Input: json.RawMessage(`{"repo":"spicedb"}`),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		workCompleteResp(),
	}
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "alice@example.com",
	}
	l, _ := makeAuthzLoop(t, tools, script, annotations)
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)
	// NOTE: Approval intentionally left nil. Permissive must still
	// allow the call through.
	l.ToolAuthMode = runner.ToolAuthModePermissive

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 1, readTool.executes, "permissive (no approval wired) should still execute")
}

// TestDispatch_PermissiveMode_ExternalStillPauses verifies that even in
// permissive mode an external-state-impact tool with NO covering slot grant
// routes through the approval flow rather than short-circuiting to Execute.
//
// The grant decides, not the mode: external checks the slot_grant leg, so a
// session holding a grant for this instance runs uninterrupted (see
// TestDispatch_ExternalWithSlotGrant_DoesNotPause) and only an ungranted call
// pauses. The fake therefore denies — with HAS_PERMISSION this would assert
// that an already-approved instance still interrupts the user. We assert this
// at the publish boundary: ApprovalPublish MUST fire for the external
// tool. The post-approval re-Check / Execute path is exercised by the
// readwrite happy-path tests; here we only care that the mode plumbing
// doesn't bypass approval for external.
func TestDispatch_PermissiveMode_ExternalStillPauses(t *testing.T) {
	spdb := &scriptedSpiceDB{
		answers: []v1.CheckPermissionResponse_Permissionship{
			v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
			v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION,
		},
	}
	extTool := &countingTool{
		name: "send_email",
		perm: authz.Permission{
			StateImpact: authz.External,
			Check: &authz.PermissionCheck{
				ResourceType:       "mailbox",
				ResourceIDTemplate: "{to}",
				Permission:         "send",
			},
		},
	}
	tools := append(meta.Load(), extTool)
	script := externalToolScript("send_email", "tu_ext", `{"to":"sue@example.com","body":"hi"}`)

	var capturedEnv channelevents.Envelope
	var capturedMu sync.Mutex
	envCh := make(chan struct{}, 1)
	l, orch := makeApprovalLoop(t, tools, script, spdb, &capturedEnv, &capturedMu, envCh)
	l.ToolAuthMode = runner.ToolAuthModePermissive

	done := make(chan error, 1)
	go func() { done <- l.Run(memory.WithSystemApproval(context.Background(), "test")) }()

	select {
	case <-envCh:
	case <-time.After(2 * time.Second):
		t.Fatal("permissive+external: expected ApprovalPublish to fire (external must always pause)")
	}

	capturedMu.Lock()
	env := capturedEnv
	capturedMu.Unlock()
	require.Equal(t, channelevents.KindInteractionRequest, env.Kind)
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	var det channelevents.ToolApprovalDetails
	require.NoError(t, json.Unmarshal(pl.Details, &det))
	assert.Equal(t, string(authz.External), det.StateImpact)
	// Drive the orchestrator so the loop unblocks (the deny path is
	// fine — we've already proven approval was requested).
	orch.DeliverDecision(pl.RequestRef, approval.Decision{Approved: false, ApproverID: "U_APPROVER"})

	select {
	case err := <-done:
		require.NoError(t, err, "loop.Run")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loop to finish")
	}
}

// TestPermissiveMode_DoesNotBypassEnforceAlways verifies the slice-4
// "no bypass" gate: a PermissionCheck with EnforceMode = EnforceAlways
// denies the tool call even when the AgentClass is in permissive mode.
// Without this gate, a permissive AgentClass could defeat per-tool
// privacy-sensitive checks (e.g. the slice-4 CRM contacts variant).
//
// Setup mirrors TestDispatch_PermissiveMode_DenyProceeds — the only
// differences are the EnforceMode stamp on the check and the assertions
// (no Execute, IsError tool result).
func TestPermissiveMode_DoesNotBypassEnforceAlways(t *testing.T) {
	readTool := &countingTool{
		name: "read_repo",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
				EnforceMode:        authz.EnforceAlways,
			},
		},
	}
	tools := append(meta.Load(), readTool)
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu_read", Name: "read_repo", Input: json.RawMessage(`{"repo":"spicedb"}`),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		workCompleteResp(),
	}
	annotations := map[string]string{
		slack.LastInboundCanonicalIDAnnotationKey: "alice@example.com",
	}
	l, _ := makeAuthzLoop(t, tools, script, annotations)
	cli := &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	l.AuthzCli = cli
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)

	l.ToolAuthMode = runner.ToolAuthModePermissive
	// Intentionally leave Approval nil — permissive normally short-
	// circuits straight through the bypass here. EnforceAlways must
	// override that bypass and surface a deny to the LLM.

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 0, readTool.executes, "EnforceAlways must NOT execute under permissive deny")
	assert.Equal(t, 1, cli.callsN, "SpiceDB should be Check'd exactly once")
}

// TestDispatch_EnforcingMode_DefaultsToEnforcing verifies that a Loop
// with ToolAuthMode left as "" (zero value) treats it as enforcing —
// the existing slice-2 behavior. A denied readonly Check WITHOUT an
// approval-publish hook surfaces IsError to the LLM, matching the
// pre-slice-2-mode path.
func TestDispatch_EnforcingMode_DefaultsToEnforcing(t *testing.T) {
	readTool := &countingTool{
		name: "read_repo",
		perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				ResourceIDTemplate: "{repo}",
				Permission:         "read",
			},
		},
	}
	tools := append(meta.Load(), readTool)
	script := []llmfake.Step{
		{Resp: llm.Response{
			Content: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu_read", Name: "read_repo", Input: json.RawMessage(`{"repo":"x"}`),
				}},
			},
			StopReason: "tool_use",
			Usage:      llm.Usage{InputTokens: 5, OutputTokens: 5},
		}},
		workCompleteResp(),
	}
	annotations := map[string]string{slack.LastInboundCanonicalIDAnnotationKey: "alice"}
	l, _ := makeAuthzLoop(t, tools, script, annotations)
	l.AuthzCli = &fakeAuthzClient{answer: v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION}
	l.AuthzCache = toolcheck.NewZedTokenCache()
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	class := &spiceboxv1alpha1.AgentClass{}
	l.ResolveAuthSubjects(sess, class)
	// ToolAuthMode intentionally unset.

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")))
	assert.Equal(t, 0, readTool.executes, "default (enforcing) deny must NOT execute")
}
