package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLoopWithFakeStatus returns a Loop backed by LocalStatusPatcher so tests
// can call l.fail without a live API server.
func newLoopWithFakeStatus(t *testing.T) *Loop {
	t.Helper()
	return &Loop{
		Status: LocalStatusPatcher(),
	}
}

// fakeStatusWroteFailed returns true if l.Status has recorded a WriteFailed call.
func fakeStatusWroteFailed(l *Loop) bool {
	return l.Status.LocalFailure() != nil
}

func TestRunnerHost_Notify_UsesLoopNotify(t *testing.T) {
	var got string
	l := &Loop{Notify: func(_ context.Context, text string) { got = text }}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.Notify(context.Background(), pipeline.Notice{Notice: testNotice("hello")})
	require.NoError(t, err)
	assert.Equal(t, "hello — Send it again.", got,
		"a text-only Host renders lead plus next step")
}

func TestRunnerHost_Notify_NilNotify_NoError(t *testing.T) {
	l := &Loop{}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.Notify(context.Background(), pipeline.Notice{Notice: testNotice("hello")})
	assert.NoError(t, err)
}

func TestRunnerHost_SetStatus_UsesNotify(t *testing.T) {
	var got string
	l := &Loop{Notify: func(_ context.Context, text string) { got = text }}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.SetStatus(context.Background(), pipeline.StatusUpdate{Text: "working"})
	require.NoError(t, err)
	assert.Equal(t, "working", got)
}

func TestRunnerHost_Halt_CallsFail(t *testing.T) {
	l := newLoopWithFakeStatus(t)
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.Halt(context.Background(), "fatal gate")
	require.NoError(t, err)
	assert.True(t, fakeStatusWroteFailed(l), "Halt must drive l.fail → WriteFailed")
}

func TestRunnerHost_Audit_NilMemory_NoError(t *testing.T) {
	l := &Loop{} // no AuditMemoryAppend
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.Audit(context.Background(), []pipeline.AuditRecord{{Kind: "read_permitted"}})
	assert.NoError(t, err)
}

// tool_call publish + approve/deny resolution + timeout are characterized in
// host_approval_tool_leakage_test.go now that the publisher emits the generic
// interaction_request (Slice C2). The reqID⇄envelope correlation (RequestRef)
// and the resource-owner Resources/Details shape are pinned there.

// TestRunnerHost_ToolCallPostApprove_ExternalSkipsRecheck pins the merge-blocker
// fix: StateImpact:External tools (e.g. apply_workspace) carry no persistent
// SpiceDB grant tuple — the human approval that reaches toolCallPostApprove IS
// the authorization. authz.check()'s External case denies unconditionally as
// the PRE-approval gate (it exists only to force the approval pause), so if
// toolCallPostApprove re-ran CheckToolCall for External the way it does for
// Readonly/Readwrite, an approved External call would ALWAYS be rejected here
// — the write-back capability would be dead even after a human approves it.
// AuthzCli is intentionally left nil: External's check() returns Denied
// before ever touching the client, so this failure mode reproduces with no
// SpiceDB wiring at all — proving the skip, not a lucky client stub.
func TestRunnerHost_ToolCallPostApprove_ExternalSkipsRecheck(t *testing.T) {
	l := &Loop{Status: LocalStatusPatcher()}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	tc := &pendingToolCall{
		sessNS:   "ns",
		sessName: "s",
		toolName: "apply_workspace",
		argsMap:  map[string]any{"op": "git_push"},
		argsHash: "hash",
		perm:     authz.Permission{StateImpact: authz.External}, // no Check
	}

	err := h.toolCallPostApprove(context.Background(), tc)
	assert.NoError(t, err, "an approved External tool call must proceed — the re-check must be skipped for External, not re-run against a Check-less permission")
}

func TestRunnerHost_LeakageApproval_DenyRecordsDenied(t *testing.T) {
	orch := approval.New()
	var published []channelevents.Envelope
	l := &Loop{
		Status:      LocalStatusPatcher(),
		Approval:    orch,
		ChannelKind: "slack",
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, ns, name string, env channelevents.Envelope) error {
			published = append(published, env)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	var deniedType, deniedID string
	// Build the leakage payload map inline — matching the production path in
	// buildLeakageApprovalAsk (pipeline_wiring.go). approver_subjects carries the
	// data #owner set (the interaction model resolves it into Audience.Approvers).
	ask := pipeline.ApprovalAsk{
		Kind: "leakage_share",
		Payload: map[string]any{
			"sess_ns":           "ns",
			"sess_name":         "s",
			"leaked_to":         []string{"user:evan@corp.example"},
			"taint":             []leakageTaintRecord{{ResourceType: "issue", ResourceID: "ENG-1", Permission: "view"}},
			"ttl":               (5 * time.Minute).String(),
			"proposed_text":     "",
			"approver":          "issue:ENG-1#owner",
			"approver_subjects": []string{"issue:ENG-1#owner"},
			"on_approved":       (func(string, string))(nil),
			"on_denied": func(rt, rid string) {
				deniedType = rt
				deniedID = rid
			},
		},
	}

	reqID, err := h.PublishApproval(context.Background(), ask)
	require.NoError(t, err)

	// Deliver the denial from a goroutine via reqID; the RequestRef correlation
	// (RequestRef == reqID on the interaction request) is asserted on the main
	// goroutine after AwaitDecision returns, so `published` is never read
	// concurrently with the OnPublish append (no -race violation).
	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: false, Reason: "not approved"})
	}()

	approved, _, _, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err)
	assert.False(t, approved)

	require.Len(t, published, 1, "should have published the leakage interaction request")
	assert.Equal(t, channelevents.KindInteractionRequest, published[0].Kind,
		"leakage_share now publishes the generic interaction_request")
	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(published[0].Payload, &pl))
	assert.Equal(t, reqID, pl.RequestRef,
		"interaction RequestRef must equal the orchestrator reqID")
	assert.False(t, pl.Audience.PublicNote, "info_leakage must never post publicly")
	assert.Equal(t, "issue", deniedType, "denied callback must record denial (runner-side effect preserved)")
	assert.Equal(t, "ENG-1", deniedID)
}

// tool_call + leakage_share timeout now publish the generic
// interaction_applied(expired) (Slice C2), and the anti-confabulation
// SYSTEM_TIMEOUT deny framing is exercised by the executor-level dispatch
// tests. The migrated timeout goldens live in
// host_approval_tool_leakage_test.go (mirroring content_inspection's move to
// host_approval_content_inspection_test.go in Slice C1).

func TestRunnerHost_ColdStartPlacement_RoundTrip(t *testing.T) {
	h := newRunnerHost(&Loop{}, hostSession{Namespace: "ns", Name: "s"})

	content := []memory.ContentBlock{{Type: "text", Text: "cleaned task"}}
	h.setColdStartPlacement(true, content)

	found, place, got := h.takeColdStartPlacement()
	assert.True(t, found, "take must report found after set")
	assert.True(t, place, "placement bool round-trips")
	require.Len(t, got, 1)
	assert.Equal(t, "cleaned task", got[0].Text, "content round-trips")
}

func TestRunnerHost_ColdStartPlacement_NotSet(t *testing.T) {
	h := newRunnerHost(&Loop{}, hostSession{Namespace: "ns", Name: "s"})
	found, _, _ := h.takeColdStartPlacement()
	assert.False(t, found, "take with nothing set must report found=false")
}

func TestRunnerHost_ColdStartPlacement_PlaceFalse_StillFound(t *testing.T) {
	h := newRunnerHost(&Loop{}, hostSession{Namespace: "ns", Name: "s"})
	h.setColdStartPlacement(false, nil)
	found, place, content := h.takeColdStartPlacement()
	assert.True(t, found, "an explicit place=false set must still report found=true (denied/empty path)")
	assert.False(t, place)
	assert.Nil(t, content)
}

func TestRunnerHost_Audit_WritesKnownKind(t *testing.T) {
	var captured []infoleakageaudit.AuditRecord
	l := &Loop{
		AuditMemoryAppend: func(_ context.Context, rec infoleakageaudit.AuditRecord) error {
			captured = append(captured, rec)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.Audit(context.Background(), []pipeline.AuditRecord{
		{Kind: "read_permitted", Fields: map[string]any{"tool": "get_issue"}},
	})
	require.NoError(t, err)
	require.Len(t, captured, 1)
	assert.Equal(t, "read_permitted", captured[0].Kind)
	assert.Equal(t, "get_issue", captured[0].Tool)
}

// TestRunnerHost_Audit_PersistsTheAttributionFailureKinds is the third instance
// of one bug, and the first one written down before it shipped: a gate emits an
// audit record, the kind is absent from writeAudit's switch, and the record is
// logged-and-dropped. The trifecta kinds and the per-datum kinds both got here
// first, and the comments beside them in host.go say so.
//
// These two are the LOGGING-MODE record of a result whose provenance could not
// be established. Logging mode's whole contract is that it changes nothing and
// reports everything, so a dropped record here makes it a control that observes
// the one event it was turned on to see.
func TestRunnerHost_Audit_PersistsTheAttributionFailureKinds(t *testing.T) {
	var captured []infoleakageaudit.AuditRecord
	l := &Loop{
		AuditMemoryAppend: func(_ context.Context, rec infoleakageaudit.AuditRecord) error {
			captured = append(captured, rec)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	kinds := []string{
		// info_leak_read: a plural declaration could not attribute the result.
		"unattributable_result",
		// info_leak_audience: the id field was unparseable, so no audience
		// record was built for the read. Pre-existing, same hole, same family.
		"audience_taint_skipped_unparseable",
	}
	for _, k := range kinds {
		require.NoError(t, h.Audit(context.Background(), []pipeline.AuditRecord{
			{Kind: k, Fields: map[string]any{"tool": "search_memory", "err": "parsing result: unexpected end of JSON input"}},
		}))
	}

	require.Len(t, captured, len(kinds), "every attribution-failure kind is persisted, not logged-and-dropped")
	for i, rec := range captured {
		assert.Equal(t, kinds[i], rec.Kind)
		assert.Equal(t, "ns/s", rec.Session)
		assert.Equal(t, "search_memory", rec.Tool)
		assert.Equal(t, "parsing result: unexpected end of JSON input", rec.Details["err"],
			"the cause is what an operator greps for")
	}
}

// TestRunnerHost_Audit_ApprovalResolved_PersistedWithApprover verifies Fix M-3:
// an "approval_resolved" AuditRecord emitted by the pipeline executor after every
// approval is persisted (not silently dropped). The approvalKind, approved, and
// approver fields must survive in Details so the audit trail captures who approved.
func TestRunnerHost_Audit_ApprovalResolved_PersistedWithApprover(t *testing.T) {
	var captured []infoleakageaudit.AuditRecord
	l := &Loop{
		AuditMemoryAppend: func(_ context.Context, rec infoleakageaudit.AuditRecord) error {
			captured = append(captured, rec)
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})
	err := h.Audit(context.Background(), []pipeline.AuditRecord{
		{
			Kind: "approval_resolved",
			Fields: map[string]any{
				"approvalKind": "tool_call",
				"approved":     true,
				"approver":     "alice",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, captured, 1, "approval_resolved must be persisted, not dropped")
	rec := captured[0]
	assert.Equal(t, "approval_resolved", rec.Kind)
	assert.Equal(t, "ns/s", rec.Session)
	require.NotNil(t, rec.Details)
	assert.Equal(t, "tool_call", rec.Details["approvalKind"])
	assert.Equal(t, "true", rec.Details["approved"])
	assert.Equal(t, "alice", rec.Details["approver"])
}

// testNotice builds a real notice for a test that only cares that SOME
// user-facing message was produced. It uses a registered category so the
// notice is valid end-to-end rather than a hand-built struct that could drift
// from what production can actually construct.
func testNotice(lead string) *notice.Notice {
	return notice.New(categories.InternalError, notice.Args{
		Lead:     lead,
		NextStep: "Send it again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}
