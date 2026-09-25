package hooks_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fullAudience(subs []string) func(context.Context) ([]string, channelkinds.Capability, error) {
	return func(context.Context) ([]string, channelkinds.Capability, error) {
		return subs, channelkinds.CapabilityFull, nil
	}
}

// memDecisionStore wires the InfoLeakAudienceDeps decision closures to a
// real infoleakage_decision memory kind over the given backend + scope, so the
// hook's approve/deny state is durable (and the restart-sim test can construct
// a fresh hook over the same backend).
func memDecisionStore(m memory.Memory, scope memory.Scope) (
	record func(ctx context.Context, resourceType, resourceID, decision string) error,
	isApproved func(ctx context.Context, resourceType, resourceID string) (bool, error),
	isDenied func(ctx context.Context, resourceType, resourceID string) (bool, error),
) {
	record = func(ctx context.Context, resourceType, resourceID, decision string) error {
		return infoleakagedecision.Record(ctx, m, scope, decision, resourceType, resourceID)
	}
	isApproved = func(ctx context.Context, resourceType, resourceID string) (bool, error) {
		return infoleakagedecision.IsApproved(ctx, m, scope, resourceType, resourceID)
	}
	isDenied = func(ctx context.Context, resourceType, resourceID string) (bool, error) {
		return infoleakagedecision.IsDenied(ctx, m, scope, resourceType, resourceID)
	}
	return record, isApproved, isDenied
}

func TestInfoLeakAudience_PostToolCall_Leak_RequestsApproval(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"bob"}),                                                                 // bob is in audience
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil }, // only alice permitted
		BuildApprovalAsk: func(_ context.Context, _ []string, _ []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	if assert.NotNil(t, dec.Approval) {
		assert.Equal(t, "leakage_share", dec.Approval.Kind)
	}
}

// TestInfoLeakAudience_PostToolCall_MalformedArgs_DeniesInEnforcing guards that
// unparseable tool args (idArg path) do NOT silently skip the audience taint
// record. In enforcing mode the parse error must Deny rather than Allow — a
// fail-open here lets an LLM hide a tainted read behind malformed JSON.
func TestInfoLeakAudience_PostToolCall_MalformedArgs_DeniesInEnforcing(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil },
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict, "unparseable args ⇒ Deny in enforcing, not silent skip")
}

// TestInfoLeakAudience_PostToolCall_MalformedResult_DeniesInEnforcing is the
// resultIDField analogue of the malformed-args case.
func TestInfoLeakAudience_PostToolCall_MalformedResult_DeniesInEnforcing(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", ResultIDField: "id"}
		},
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil },
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{}}`), Result: `{"id":`},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict, "unparseable result ⇒ Deny in enforcing, not silent skip")
}

// TestInfoLeakAudience_PostToolCall_MalformedArgs_AllowsInLogging confirms the
// logging-vs-enforcing distinction: in logging mode the parse error is NOT a
// hard deny (logging mode never blocks). A nil-err empty value also stays a
// legit skip in either mode (covered by the no-id passthrough below).
func TestInfoLeakAudience_PostToolCall_MalformedArgs_AllowsInLogging(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "logging",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil },
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":`), Result: `{}`},
	})
	assert.NotEqual(t, pipeline.Deny, dec.Verdict, "logging mode never hard-denies on parse error")
}

// TestInfoLeakAudience_PostToolCall_LoggingLeak_PublishesNotice pins the
// Task-11 (D2) flip: a leak detected in "logging" mode is auto-allowed
// (never Denies) AND, when NoticeToRequester is set, invokes PublishNotice
// with the leaked-to audience + causative taint — the new info_leakage_notice
// producer path.
func TestInfoLeakAudience_PostToolCall_LoggingLeak_PublishesNotice(t *testing.T) {
	var gotRequester identity.CanonicalUserID
	var gotLeakedTo []string
	var gotTaint []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "logging",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience:   fullAudience([]string{"bob"}),                                                                 // bob is in audience
		LookupSubjects:    func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil }, // only alice permitted
		NoticeToRequester: true,
		PublishNotice: func(_ context.Context, requester identity.CanonicalUserID, leakedTo []string, taint []infoleakagetaint.TaintRecord) error {
			gotRequester = requester
			gotLeakedTo = leakedTo
			gotTaint = taint
			return nil
		},
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict, "logging mode never blocks — the leak is auto-allowed")
	assert.Nil(t, dec.Approval, "logging mode never pauses for approval")
	require.Len(t, dec.Audit, 1)
	assert.Equal(t, "would_block_leakage", dec.Audit[0].Kind)

	assert.Equal(t, identity.CanonicalFromTrusted("alice", "test fixture"), gotRequester,
		"PublishNotice must receive the PER-CALL principal (pipeline.Input.Requester), not a session-resolved one")
	assert.Equal(t, []string{"bob"}, gotLeakedTo, "PublishNotice must receive the leaked-to audience")
	require.Len(t, gotTaint, 1, "PublishNotice must receive the causative taint")
	assert.Equal(t, "issue", gotTaint[0].ResourceType)
	assert.Equal(t, "ENG-1", gotTaint[0].ResourceID)
}

// TestInfoLeakAudience_PostToolCall_LoggingLeak_NoticeSuppressedWhenGateOff
// verifies NoticeToRequester=false suppresses the notice (mirrors
// informationLeakage.loggingNoticeToRequester=false) even though a nil
// PublishNotice closure would already no-op it — both gates must be
// independently effective.
func TestInfoLeakAudience_PostToolCall_LoggingLeak_NoticeSuppressedWhenGateOff(t *testing.T) {
	called := false
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "logging",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience:   fullAudience([]string{"bob"}),
		LookupSubjects:    func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil },
		NoticeToRequester: false,
		PublishNotice: func(context.Context, identity.CanonicalUserID, []string, []infoleakagetaint.TaintRecord) error {
			called = true
			return nil
		},
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.False(t, called, "NoticeToRequester=false must suppress the notice publish")
}

func TestInfoLeakAudience_PostToolCall_NoLeak_Allows(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice", "bob"}, nil },
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Nil(t, dec.Approval)
}

func TestInfoLeakAudience_PreResponse_PreviouslyDenied_DeniesWithoutReprompt(t *testing.T) {
	// First a PostToolCall leak that the executor denies → hook records denial.
	// Then a PreResponse over the same taint must Deny (wrapping ErrShareDenied)
	// WITHOUT a new ApprovalAsk. Modeled by seeding the denied set then evaluating PreResponse.
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	record, isApproved, isDenied := memDecisionStore(m, scope)
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:            "enforcing",
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil },
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return []infoleakagetaint.TaintRecord{{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"}}, nil
		},
		RecordDecision: record,
		IsApproved:     isApproved,
		IsDenied:       isDenied,
	})
	h.RecordDeniedForTest("issue", "ENG-1")
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is ENG-1"},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Nil(t, dec.Approval, "already-denied resource is NOT re-prompted")
	assert.Contains(t, dec.Reason, "ErrShareDenied") // the host detects this marker to yield
}

func TestInfoLeakAudience_DisabledMode_NoOp(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{Mode: "disabled"})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

func TestInfoLeakAudience_Points(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{Mode: "enforcing"})
	// PreToolCall joined the set with per-datum provenance. A tool call carries
	// data OUT of the session exactly as a reply does — its audience is whoever
	// can read the destination — but under a session-wide taint set that
	// question had no useful answer, so the checkpoint did not exist. It is
	// inert unless FineGrained is wired AND the capability is granted, which
	// TestToolCallCheckpointIsInertWithoutWiring pins.
	assert.ElementsMatch(t,
		[]pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall, pipeline.PreResponse},
		h.Points())
	assert.Equal(t, "info_leak_audience", h.Name())
}

func TestInfoLeakAudience_PreResponse_NoTaint_NoLeak(t *testing.T) {
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:            "enforcing",
		ResolveAudience: fullAudience([]string{"bob"}),
		TaintList:       func(context.Context) ([]infoleakagetaint.TaintRecord, error) { return nil, nil },
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "hello"},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

// TestInfoLeakAudience_PostToolCall_Approve_SuppressesPreResponse verifies that
// once the PostToolCall gate approves a resource for the session (by marking it
// approved via markApproved), the PreResponse gate skips that resource so the
// approver is NOT re-prompted. The coverage belongs here because
// InfoLeakAudience.evalPostToolCall and evalPreResponse share one hook instance
// and one pair of approved/denied sets.
func TestInfoLeakAudience_PostToolCall_Approve_SuppressesPreResponse(t *testing.T) {
	awaitCallCount := 0

	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	record, isApproved, isDenied := memDecisionStore(m, scope)
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"user:alice", "user:bob"}),
		LookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			// only alice is permitted; bob would be leaked
			return []string{"user:alice"}, nil
		},
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return []infoleakagetaint.TaintRecord{{
				ToolUseID: "tu-1", ResourceType: "issue", ResourceID: "ENG-1", Permission: "view",
			}}, nil
		},
		BuildApprovalAsk: func(_ context.Context, _ []string, _ []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
			awaitCallCount++
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
		RecordDecision: record,
		IsApproved:     isApproved,
		IsDenied:       isDenied,
	})

	// 1. PostToolCall fires for the tool call → approval ask requested (#1).
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), UseID: "tu-1"},
	})
	// Executor resolves the ask: simulate the host recording approval.
	h.RecordApproved("issue", "ENG-1")
	assert.NotNil(t, dec.Approval, "pre-feed gate should have requested approval")
	assert.Equal(t, 1, awaitCallCount, "BuildApprovalAsk called once at PostToolCall")

	// 2. Later, the agent responds. The PreResponse gate reads the accumulated
	// taint (ENG-1 still there) but ENG-1 is now marked approved — must skip.
	dec2 := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is ENG-1 info"},
	})
	assert.Equal(t, pipeline.Allow, dec2.Verdict,
		"PreResponse must NOT re-prompt: resource was already approved at PostToolCall")
	assert.Equal(t, 1, awaitCallCount,
		"BuildApprovalAsk must NOT be called again on PreResponse for already-approved resource")
}

// TestInfoLeakAudience_StaleTaint_AfterRestart_DoesNotSurfaceGrantedResources
// verifies that resources already granted in SpiceDB (from a prior session or
// runner restart) are filtered out of the PreResponse approval prompt, so the
// user is never asked twice for the same leakage event. The coverage belongs
// here because the stale-taint filter (filterTaintCausedByAudience) is part of
// InfoLeakAudience.enforceAudienceForTaint.
func TestInfoLeakAudience_StaleTaint_AfterRestart_DoesNotSurfaceGrantedResources(t *testing.T) {
	// After restart: L-140 is granted (evan permitted), L-123 is not yet granted.
	lookupSubjects := func(_ context.Context, resource, _ string) ([]string, error) {
		switch resource {
		case "issue:L-140":
			return []string{"user:evan"}, nil // SpiceDB grant already exists
		case "issue:L-123":
			return []string{}, nil // no grant yet
		}
		return []string{}, nil
	}

	var capturedTaint []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:            "enforcing",
		ResolveAudience: fullAudience([]string{"user:evan"}),
		LookupSubjects:  lookupSubjects,
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return []infoleakagetaint.TaintRecord{
				{ToolUseID: "tu-140", ResourceType: "issue", ResourceID: "L-140", Permission: "view"},
				{ToolUseID: "tu-123", ResourceType: "issue", ResourceID: "L-123", Permission: "view"},
			}, nil
		},
		BuildApprovalAsk: func(_ context.Context, _ []string, taint []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
			capturedTaint = taint
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
	})

	// approvedLeakageResources is intentionally NOT pre-populated on the hook
	// (simulates a runner restart where the in-memory approved set was cleared).

	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is the L-123 answer"},
	})
	// The approval ask is expected (L-123 is not yet granted).
	assert.NotNil(t, dec.Approval, "approval should be requested for L-123")

	// LOAD-BEARING: the approval ask must contain ONLY L-123, NOT L-140.
	// L-140 was previously granted (SpiceDB returned evan as permitted),
	// so it must not appear in the prompt.
	var resourceIDs []string
	for _, r := range capturedTaint {
		resourceIDs = append(resourceIDs, r.ResourceType+":"+r.ResourceID)
	}
	assert.NotContains(t, resourceIDs, "issue:L-140",
		"L-140 is already granted; must NOT appear in the approval prompt (stale-taint replay bug)")
	assert.Contains(t, resourceIDs, "issue:L-123",
		"L-123 is not yet granted; MUST appear in the approval prompt")
}

// TestInfoLeakAudience_PostToolCall_TaintRecord_CarriesUseID verifies Fix I1:
// a PostToolCall input with UseID="tu-1" produces an approval ask whose
// causative synthetic taint record carries ToolUseID == "tu-1".
// We confirm this by capturing what BuildApprovalAsk receives.
func TestInfoLeakAudience_PostToolCall_TaintRecord_CarriesUseID(t *testing.T) {
	var capturedTaint []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  func(_ context.Context, res, perm string) ([]string, error) { return []string{"alice"}, nil },
		BuildApprovalAsk: func(_ context.Context, _ []string, taint []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
			capturedTaint = taint
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
	})
	dec := h.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name:  "get_issue",
			Args:  json.RawMessage(`{"args":{"id":"ENG-1"}}`),
			UseID: "tu-1",
		},
	})
	assert.NotNil(t, dec.Approval)
	require.Len(t, capturedTaint, 1)
	assert.Equal(t, "tu-1", capturedTaint[0].ToolUseID,
		"ToolUseID must flow from pipeline.ToolCallInfo.UseID into the synthetic taint record (Fix I1)")
}

// TestInfoLeakAudience_Denied_SurvivesFreshHook is the restart-sim bar for
// Sub-stream 2: hook instance A records a DENY for issue:ENG-1 through the
// memory-backed decision store; a FRESH hook instance B over the SAME backend
// must still short-circuit Deny (no re-prompt) for causative taint on ENG-1.
// An in-process map would have lost this across the restart.
func TestInfoLeakAudience_Denied_SurvivesFreshHook(t *testing.T) {
	backend := inmem.NewBackend()
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	taintList := func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
		return []infoleakagetaint.TaintRecord{{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"}}, nil
	}
	lookupSubjects := func(_ context.Context, _, _ string) ([]string, error) { return []string{"alice"}, nil }

	// Instance A ("before restart"): record a denial.
	mA := memory.NewLocal(backend)
	recordA, isApprovedA, isDeniedA := memDecisionStore(mA, scope)
	hookA := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:            "enforcing",
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  lookupSubjects,
		TaintList:       taintList,
		RecordDecision:  recordA,
		IsApproved:      isApprovedA,
		IsDenied:        isDeniedA,
	})
	hookA.RecordDenied("issue", "ENG-1")

	// Instance B ("after restart"): fresh hook + memory handle over the SAME backend.
	mB := memory.NewLocal(backend)
	recordB, isApprovedB, isDeniedB := memDecisionStore(mB, scope)
	hookB := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode:            "enforcing",
		ResolveAudience: fullAudience([]string{"bob"}),
		LookupSubjects:  lookupSubjects,
		TaintList:       taintList,
		RecordDecision:  recordB,
		IsApproved:      isApprovedB,
		IsDenied:        isDeniedB,
		// No BuildApprovalAsk: a re-prompt would surface as a fail-closed Deny
		// WITHOUT the ErrShareDenied marker, so the assertion below distinguishes
		// "remembered the prior deny" from "couldn't ask again".
	})
	dec := hookB.Eval(memory.WithSystemApproval(context.Background(), "test"), pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is ENG-1"},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Nil(t, dec.Approval, "already-denied resource is NOT re-prompted by a fresh hook")
	assert.Contains(t, dec.Reason, "ErrShareDenied",
		"fresh hook must read the durable denial and short-circuit (restart sim)")
}

// TestInfoLeakAudience_AudienceGrowsAfterRead_ReGatesAtRespond pins the
// audience-side invariant the whole hook exists for: "nobody unpermitted was
// watching when I read it" is an observation about ONE moment, not a standing
// exemption. A read that leaked to nobody must not record a durable approval,
// because the audience can grow between the read and the reply.
//
// Three turns: the agent reads issue:ENG-1 while the channel audience is only
// alice (who has view) — no leak, no prompt; bob then joins the channel with no
// view on ENG-1; the agent replies with the contents. The respond-time gate must
// re-gate and ask, not wave the resource through on a cached "approved".
func TestInfoLeakAudience_AudienceGrowsAfterRead_ReGatesAtRespond(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	record, isApproved, isDenied := memDecisionStore(m, scope)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	audience := []string{"user:alice"} // mutated between turns as bob joins
	askCount := 0
	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: func(context.Context) ([]string, channelkinds.Capability, error) {
			return audience, channelkinds.CapabilityFull, nil
		},
		// Only alice ever holds view on ENG-1; bob never gains it.
		LookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:alice"}, nil
		},
		TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
			return []infoleakagetaint.TaintRecord{{
				ToolUseID: "tu-1", ResourceType: "issue", ResourceID: "ENG-1", Permission: "view",
			}}, nil
		},
		BuildApprovalAsk: func(_ context.Context, _ []string, _ []infoleakagetaint.TaintRecord, _ string) (*pipeline.ApprovalAsk, error) {
			askCount++
			return &pipeline.ApprovalAsk{Kind: "leakage_share"}, nil
		},
		RecordDecision: record,
		IsApproved:     isApproved,
		IsDenied:       isDenied,
	})

	// Turn 1: read ENG-1 with only alice watching. Permitted ⇒ no leak, no ask.
	dec := h.Eval(ctx, pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), UseID: "tu-1"},
	})
	require.Equal(t, pipeline.Allow, dec.Verdict, "no unpermitted audience member ⇒ allow")
	require.Nil(t, dec.Approval, "no leak ⇒ no approval ask")
	require.Equal(t, 0, askCount)

	// An approval nobody was ever asked for must not be on record: the decision
	// store is append-only security evidence, not a cache of momentary verdicts.
	approvedAfterRead, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.False(t, approvedAfterRead,
		"a read that leaked to nobody must NOT record DecisionApproved: no approver ever decided")

	// Turn 2: bob joins the channel. He has no view on ENG-1.
	audience = []string{"user:alice", "user:bob"}

	// Turn 3: the agent replies with the contents.
	dec2 := h.Eval(ctx, pipeline.Input{
		Point:    pipeline.PreResponse,
		Response: &pipeline.ResponseInfo{Text: "here is ENG-1"},
	})
	assert.NotNil(t, dec2.Approval,
		"audience grew after the read: the respond-time gate must re-gate and ask, not reuse a stale no-leak result")
	assert.Equal(t, 1, askCount, "exactly one approval ask, raised at respond time")
}

// TestInfoLeakAudience_LoggingMode_DoesNotRecordApproval pins logging mode as
// observe-only on the WRITE side too. Its would-block event is auto-allowed
// (Verdict Allow, no ask), which must not be mistaken for an approval and
// written into the durable decision store — that would both fabricate evidence
// and, via filterApproved, suppress the respond-time would-block audit that
// makes logging mode useful.
func TestInfoLeakAudience_LoggingMode_DoesNotRecordApproval(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	record, isApproved, isDenied := memDecisionStore(m, scope)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	h := hooks.NewInfoLeakAudience(hooks.InfoLeakAudienceDeps{
		Mode: "logging",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		ResolveAudience: fullAudience([]string{"user:bob"}),                                                            // bob is watching
		LookupSubjects:  func(_ context.Context, _, _ string) ([]string, error) { return []string{"user:alice"}, nil }, // only alice permitted
		RecordDecision:  record,
		IsApproved:      isApproved,
		IsDenied:        isDenied,
	})
	dec := h.Eval(ctx, pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	require.Equal(t, pipeline.Allow, dec.Verdict, "logging mode never blocks")
	require.Len(t, dec.Audit, 1)
	require.Equal(t, "would_block_leakage", dec.Audit[0].Kind)

	approved, err := infoleakagedecision.IsApproved(ctx, m, scope, "issue", "ENG-1")
	require.NoError(t, err)
	assert.False(t, approved,
		"logging mode is observe-only: a would-block must not write DecisionApproved")
}
