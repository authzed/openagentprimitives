package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
)

// fakeChecker returns a scripted authz.Result and records the Inputs it saw.
type fakeChecker struct {
	result authz.Result
	gotIn  authz.Inputs
	gotP   authz.Permission
}

func (f *fakeChecker) CheckToolCall(_ context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	f.gotP, f.gotIn = p, in
	return f.result
}

func permResolverFor(p authz.Permission) func(string, map[string]any) (authz.Permission, error) {
	return func(string, map[string]any) (authz.Permission, error) { return p, nil }
}

func simpleApprovalAskBuilder(_ context.Context, in pipeline.Input, _ map[string]any, _ authz.Permission, _ string) (*pipeline.ApprovalAsk, error) {
	return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve " + in.Tool.Name}, nil
}

// simpleWaiverAskBuilder stands in for the runner's buildPreconditionWaiverAsk.
// Its Summary is the DENIAL's own Message, so a test asserting the Summary
// carries the RefusalMessage proves the hook threaded the gate-authored denial
// through to the builder — not that this stub hardcoded the sentence. The Kind
// is the shared categories.PreconditionWaiver constant (RULING P3-1), the same
// symbol the real builder, the PublishApproval switch, and the payload Category
// use.
func simpleWaiverAskBuilder(_ context.Context, _ pipeline.Input, _ map[string]any, _ authz.Permission, denial *authz.PreconditionDenial) (*pipeline.ApprovalAsk, error) {
	return &pipeline.ApprovalAsk{Kind: categories.PreconditionWaiver, Summary: denial.Message}, nil
}

func TestToolCallAuthz_DisabledMode_Allows(t *testing.T) {
	chk := &fakeChecker{}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:              "disabled",
		Checker:           chk,
		ResolvePermission: permResolverFor(authz.Permission{StateImpact: authz.Readonly}),
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, chk.gotP.StateImpact, "disabled mode must NOT call the checker")
}

func TestToolCallAuthz_Allowed_Allows(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
		}),
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

func TestToolCallAuthz_DeniedReadonly_RequestsApproval(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: "permission denied"}}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
		}),
		BuildApprovalAsk: simpleApprovalAskBuilder,
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict, "approval ask carries the gate; verdict stays Allow until the executor resolves it")
	if assert.NotNil(t, dec.Approval) {
		assert.Equal(t, "tool_call", dec.Approval.Kind)
	}
}

func TestToolCallAuthz_PermissiveDeniedReadonly_AllowsNoApproval(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: "permission denied"}}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "permissive",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
		}),
		BuildApprovalAsk: simpleApprovalAskBuilder,
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Nil(t, dec.Approval, "permissive readonly deny logs + proceeds; no approval ask")
}

func TestToolCallAuthz_ExternalAlwaysAsksApproval(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: "external"}}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "permissive", // external still asks even in permissive
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.External,
			Check:       &authz.PermissionCheck{ResourceType: "ext", Permission: "use"},
		}),
		BuildApprovalAsk: simpleApprovalAskBuilder,
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.NotNil(t, dec.Approval)
}

func TestToolCallAuthz_PermissionResolutionError_DeniesFailClosed(t *testing.T) {
	chk := &fakeChecker{}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: func(string, map[string]any) (authz.Permission, error) {
			return authz.Permission{}, errors.New("CEL compile error")
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "fail-closed")
}

func TestToolCallAuthz_Points(t *testing.T) {
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{Mode: "disabled"})
	assert.Equal(t, []pipeline.Point{pipeline.PreToolCall}, h.Points())
	assert.Equal(t, "tool_call_authz", h.Name())
}

// TestNeedsApproval_Matrix enumerates the (Result outcome × Permission
// state-impact) matrix for the approval gate, against this package's own
// private needsApproval.
//
// Truth table:
//   - Denied × {Readonly, Readwrite} → approval required (JIT grant on approve)
//   - Denied × Stateless              → no approval (Check never runs)
//   - Denied × External               → approval required
//   - Allowed × External              → NO approval. External's check consults
//     the slot-grant leg ALONE, so an allowed result means a human cleared a
//     card naming this instance and permission for this session — not that the
//     requester owns the resource. See toolcheck.checkExternalSlotGrant.
//   - Allowed × {Readonly, Readwrite} → no approval (pass through)
func TestNeedsApproval_Matrix(t *testing.T) {
	denied := authz.Result{Outcome: authz.OutcomeDenied, Message: "permission denied"}
	allowed := authz.Result{Outcome: authz.OutcomeAllowed}
	unresolved := authz.Result{
		Outcome:            authz.OutcomeDenied,
		Message:            "this call did not identify which github_repo it acts on",
		UnresolvedResource: true,
	}

	readonlyPerm := authz.Permission{
		StateImpact: authz.Readonly,
		Check:       &authz.PermissionCheck{ResourceType: "github_repo", Permission: "read"},
	}
	readwritePerm := authz.Permission{
		StateImpact: authz.Readwrite,
		Check:       &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write"},
	}
	statelessPerm := authz.Permission{StateImpact: authz.Stateless}
	passthroughPerm := authz.Permission{StateImpact: authz.Passthrough}
	externalPerm := authz.Permission{
		StateImpact: authz.External,
		Check:       &authz.PermissionCheck{ResourceType: "github_repo", Permission: "write"},
	}

	cases := []struct {
		name    string
		result  authz.Result
		perm    authz.Permission
		wantAsk bool
	}{
		{"denied readonly: needs approval", denied, readonlyPerm, true},
		{"denied readwrite: needs approval", denied, readwritePerm, true},
		// Stateless never runs Check, so a "denied" Result shouldn't happen —
		// but if it does, the policy is "no approval prompt".
		{"denied stateless: no approval (Check never runs)", denied, statelessPerm, false},
		{"denied external: needs approval", denied, externalPerm, true},
		{"allowed external: no approval — an allowed external IS a slot grant a human wrote", allowed, externalPerm, false},
		{"allowed readwrite: passes through", allowed, readwritePerm, false},
		{"allowed readonly: passes through", allowed, readonlyPerm, false},
		// Passthrough touches state but maps to no SpiceDB resource: CheckToolCall
		// returns Allowed and the call NEVER requests approval — absent any interact/
		// owner policy, a passthrough-only agent (e.g. git + claude) must pause for
		// nothing. (Pins the contract behind the stranded-AwaitingApproval fix.)
		{"allowed passthrough: passes through (no approval)", allowed, passthroughPerm, false},
		{"denied passthrough: no approval (Check never runs)", denied, passthroughPerm, false},
		// The call never named its resource, so there is nothing to approve: the
		// card would ask a human about `github_repo:` with an EMPTY object id,
		// and granting it cannot help — the retry fails at resolution again,
		// before SpiceDB is consulted. This denial belongs back with the agent,
		// which is the only party that can fix it.
		{"unresolved resource, readwrite: no approval", unresolved, readwritePerm, false},
		{"unresolved resource, readonly: no approval", unresolved, readonlyPerm, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := &fakeChecker{result: tc.result}
			h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
				Mode:              "enforcing",
				Checker:           chk,
				ResolvePermission: permResolverFor(tc.perm),
				BuildApprovalAsk:  simpleApprovalAskBuilder,
			})
			dec := h.Eval(context.Background(), pipeline.Input{
				Point: pipeline.PreToolCall,
				Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{"args":{}}`)},
			})
			if tc.wantAsk {
				assert.NotNil(t, dec.Approval, "expected an approval ask for case %q", tc.name)
			} else {
				assert.Nil(t, dec.Approval, "did not expect an approval ask for case %q", tc.name)
			}
		})
	}
}

// TestToolCallAuthz_ForceApproval_EscalatesAllowedTool verifies that when
// ForceApproval returns ok=true for a tool that would otherwise be allowed
// (e.g. a readonly tool with a passing SpiceDB check), the hook escalates it
// to an ApprovalAsk with the drift reason embedded in the justification.
func TestToolCallAuthz_ForceApproval_EscalatesAllowedTool(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	var capturedJustification string
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
		}),
		BuildApprovalAsk: func(_ context.Context, _ pipeline.Input, _ map[string]any, _ authz.Permission, justification string) (*pipeline.ApprovalAsk, error) {
			capturedJustification = justification
			return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve it"}, nil
		},
		ForceApproval: func(toolName string) (string, bool) {
			if toolName == "t" {
				return "MCPServer/gh drifted sha256:old -> sha256:new", true
			}
			return "", false
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{}`), Justification: "I need this data"},
	})
	if assert.NotNil(t, dec.Approval, "ForceApproval=ok must produce an ApprovalAsk even for an allowed readonly tool") {
		assert.Equal(t, "tool_call", dec.Approval.Kind)
	}
	assert.Contains(t, capturedJustification, "PIN DRIFT",
		"forced drift reason must appear in the justification passed to BuildApprovalAsk")
	assert.Contains(t, capturedJustification, "I need this data",
		"the agent's original justification must be preserved")
}

// TestToolCallAuthz_ForceApproval_NotOk_Unchanged verifies that when
// ForceApproval returns ok=false the call proceeds without escalation.
func TestToolCallAuthz_ForceApproval_NotOk_Unchanged(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
		}),
		BuildApprovalAsk: simpleApprovalAskBuilder,
		ForceApproval: func(string) (string, bool) {
			return "", false // never matches
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{}`)},
	})
	assert.Nil(t, dec.Approval, "ForceApproval=not-ok must not escalate an allowed tool")
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

// TestToolCallAuthz_ForceApproval_PermissiveMode_StillAsks verifies that when
// ForceApproval returns ok=true the permissive bypass is skipped — the operator
// explicitly chose "approve" drift mode, so we must always pause for the human
// even though the base decision would be permissively allowed.
func TestToolCallAuthz_ForceApproval_PermissiveMode_StillAsks(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	var capturedJustification string
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "permissive",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
		}),
		BuildApprovalAsk: func(_ context.Context, _ pipeline.Input, _ map[string]any, _ authz.Permission, justification string) (*pipeline.ApprovalAsk, error) {
			capturedJustification = justification
			return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve it"}, nil
		},
		ForceApproval: func(string) (string, bool) {
			return "MCPServer/gh drifted sha256:old -> sha256:new", true
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{}`)},
	})
	if assert.NotNil(t, dec.Approval, "forced approval must fire even in permissive mode") {
		assert.Equal(t, "tool_call", dec.Approval.Kind)
	}
	assert.Contains(t, capturedJustification, "PIN DRIFT",
		"forceReason guard: justification must carry the drift marker in permissive mode")
}

// TestToolCallAuthz_ForceApproval_AlreadyNeedsApproval_CarriesDriftMarker
// verifies that when a tool already needs approval (external stateImpact) AND
// ForceApproval returns ok=true, the approval ask still carries the [PIN DRIFT: …]
// marker — i.e. ForceApproval is consulted unconditionally, not only when the
// base result was "no approval needed".
func TestToolCallAuthz_ForceApproval_AlreadyNeedsApproval_CarriesDriftMarker(t *testing.T) {
	// External stateImpact → always needs approval regardless of check outcome.
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	var capturedJustification string
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.External,
			Check:       &authz.PermissionCheck{ResourceType: "ext", Permission: "use"},
		}),
		BuildApprovalAsk: func(_ context.Context, _ pipeline.Input, _ map[string]any, _ authz.Permission, justification string) (*pipeline.ApprovalAsk, error) {
			capturedJustification = justification
			return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve it"}, nil
		},
		ForceApproval: func(string) (string, bool) {
			return "MCPServer/gh drifted sha256:aaa -> sha256:bbb", true
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "t", Args: json.RawMessage(`{}`), Justification: "need data"},
	})
	if assert.NotNil(t, dec.Approval, "external stateImpact + ForceApproval must produce an approval ask") {
		assert.Equal(t, "tool_call", dec.Approval.Kind)
	}
	assert.Contains(t, capturedJustification, "PIN DRIFT",
		"already-gated external call must also carry the drift marker when dependency drifted")
	assert.Contains(t, capturedJustification, "need data",
		"original agent justification must be preserved")
}

// TestToolCallAuthz_BuildApprovalAsk_CarriesUseIDAndJustification verifies
// Fix I1+M1: when in.Tool.UseID and in.Tool.Justification are set, they are
// forwarded to BuildApprovalAsk verbatim.
func TestToolCallAuthz_BuildApprovalAsk_CarriesUseIDAndJustification(t *testing.T) {
	chk := &fakeChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: "permission denied"}}
	var capturedUseID, capturedJustification string
	h := hooks.NewToolCallAuthz(hooks.ToolCallAuthzDeps{
		Mode:    "enforcing",
		Checker: chk,
		ResolvePermission: permResolverFor(authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "issue", Permission: "view"},
		}),
		BuildApprovalAsk: func(_ context.Context, in pipeline.Input, _ map[string]any, _ authz.Permission, justification string) (*pipeline.ApprovalAsk, error) {
			capturedUseID = in.Tool.UseID
			capturedJustification = justification
			return &pipeline.ApprovalAsk{Kind: "tool_call", Summary: "approve " + in.Tool.Name}, nil
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name:          "get_issue",
			Args:          json.RawMessage(`{"args":{}}`),
			UseID:         "tu-42",
			Justification: "need to check issue status",
		},
	})
	assert.NotNil(t, dec.Approval, "denied readonly tool should trigger approval ask")
	assert.Equal(t, "tu-42", capturedUseID,
		"UseID must flow from pipeline.ToolCallInfo.UseID into BuildApprovalAsk (Fix I1)")
	assert.Equal(t, "need to check issue status", capturedJustification,
		"Justification must flow from pipeline.ToolCallInfo.Justification into BuildApprovalAsk (Fix I1)")
}
