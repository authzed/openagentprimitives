package pipelinehost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/coldstart"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/coldstarttask"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// fakeOrch records the published envelope and returns a scripted decision.
type fakeOrch struct {
	awaits     int
	published  []byte
	publishErr error
	dec        approval.Decision
	awaitErr   error
}

func (f *fakeOrch) Await(ctx context.Context, r approval.Request) (approval.Decision, error) {
	f.awaits++
	if r.OnPublish != nil {
		if err := r.OnPublish(ctx); err != nil {
			return approval.Decision{}, err
		}
	}
	if f.awaitErr != nil {
		return approval.Decision{Reason: "err"}, f.awaitErr
	}
	return f.dec, nil
}

type appliedCall struct {
	delta scope.ScopeDelta
}

type writtenTask struct {
	content coldstarttask.Content
}

func newColdStartHost(t *testing.T, orch *fakeOrch) (*Host, *[]appliedCall, *[]writtenTask, *[]string) {
	t.Helper()
	var applies []appliedCall
	var writes []writtenTask
	var notices []string
	h := New(Deps{
		Session:      SessionRef{Namespace: "ns", Name: "a"},
		Orchestrator: orch,
		Publish: func(_ context.Context, payload []byte) error {
			orch.published = payload
			return orch.publishErr
		},
		ApplyScope: func(_ context.Context, d scope.ScopeDelta) error {
			applies = append(applies, appliedCall{delta: d})
			return nil
		},
		WriteTask: func(_ context.Context, c coldstarttask.Content) error {
			writes = append(writes, writtenTask{content: c})
			return nil
		},
		NotifyRequester: func(_ context.Context, body string) {
			notices = append(notices, body)
		},
	})
	return h, &applies, &writes, &notices
}

func coldStartAsk() pipeline.ApprovalAsk {
	return pipeline.ApprovalAsk{
		Kind:    "cold_start",
		Summary: "scope review",
		Payload: map[string]any{
			"requester":       "user:alice",
			"verbatim":        "summarize L-140 and do not read ENG",
			"cleanedTask":     "summarize L-140",
			"approverSummary": "scoped.",
			"skippedExplain":  "",
			"caveatExplain":   "",
			"applied":         scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
			"skipped":         []scope.SkippedItem{},
			"caveats":         []scope.CaveatItem{},
			"inboxIdx":        0,
		},
	}
}

// TestHost_PublishApproval_MintsReqID verifies a reqID is minted and pending
// state stored (the envelope publish itself happens inside AwaitDecision).
func TestHost_PublishApproval_MintsReqID(t *testing.T) {
	orch := &fakeOrch{}
	h, _, _, _ := newColdStartHost(t, orch)
	reqID, err := h.PublishApproval(context.Background(), coldStartAsk())
	require.NoError(t, err)
	assert.NotEmpty(t, reqID)
	assert.Equal(t, 0, orch.awaits, "PublishApproval must NOT await yet")
}

// TestHost_AwaitDecision_PayloadShape pins the metaagent_scope_approval payload
// field names, which channelsd renders its five approval buttons off.
func TestHost_AwaitDecision_PayloadShape(t *testing.T) {
	orch := &fakeOrch{dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveCleaned, ApproverID: "user:bob"}}
	h, _, _, _ := newColdStartHost(t, orch)
	reqID, err := h.PublishApproval(context.Background(), coldStartAsk())
	require.NoError(t, err)
	_, _, _, err = h.AwaitDecision(context.Background(), reqID, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, orch.published, "the envelope must be published inside Await's OnPublish")

	var p map[string]any
	require.NoError(t, json.Unmarshal(orch.published, &p))
	for _, key := range []string{
		"requestId", "requester", "verbatim", "coldStart", "cleanedTask",
		"approverSummary", "skippedExplain", "caveatExplain", "applied", "skipped", "caveats",
	} {
		_, ok := p[key]
		assert.Truef(t, ok, "payload must carry %q (byte-identical to makeColdStartDecider)", key)
	}
	assert.Equal(t, true, p["coldStart"], "coldStart must be true")
	assert.Equal(t, reqID, p["requestId"], "requestId must equal the minted reqID (envelope/await key match)")
	assert.Equal(t, "summarize L-140", p["cleanedTask"])
}

// TestHost_AwaitDecision_ActionEffects table-drives the five-way action mapping
// (owned by the host's AwaitDecision, mirroring runnerHost's effect-in-await).
func TestHost_AwaitDecision_ActionEffects(t *testing.T) {
	cases := []struct {
		name         string
		dec          approval.Decision
		awaitErr     error
		wantApproved bool
		wantApply    bool
		wantStatus   string
	}{
		{
			name:         "approve_cleaned: apply + StatusApprovedCleaned, approved",
			dec:          approval.Decision{Approved: true, Action: coldstart.ActionApproveCleaned},
			wantApproved: true, wantApply: true, wantStatus: coldstarttask.StatusApprovedCleaned,
		},
		{
			name:         "approve_original: apply + StatusApprovedOriginal, approved",
			dec:          approval.Decision{Approved: true, Action: coldstart.ActionApproveOriginal},
			wantApproved: true, wantApply: true, wantStatus: coldstarttask.StatusApprovedOriginal,
		},
		{
			name:         "run_without_scope: no apply + StatusRanWithoutScope, not approved",
			dec:          approval.Decision{Approved: true, Action: coldstart.ActionRunWithoutScope},
			wantApproved: false, wantApply: false, wantStatus: coldstarttask.StatusRanWithoutScope,
		},
		{
			name:         "deny: no apply + StatusDenied, not approved",
			dec:          approval.Decision{Approved: false, Action: coldstart.ActionDeny},
			wantApproved: false, wantApply: false, wantStatus: coldstarttask.StatusDenied,
		},
		{
			name:         "await error → deny (StatusDenied via ActionForDecision fallback)",
			awaitErr:     errors.New("orch timeout"),
			wantApproved: false, wantApply: false, wantStatus: coldstarttask.StatusDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := &fakeOrch{dec: tc.dec, awaitErr: tc.awaitErr}
			h, applies, writes, _ := newColdStartHost(t, orch)
			reqID, err := h.PublishApproval(context.Background(), coldStartAsk())
			require.NoError(t, err)
			approved, _, _, err := h.AwaitDecision(context.Background(), reqID, time.Minute)
			require.NoError(t, err, "await error maps to deny (approved=false), NOT a host error")
			assert.Equal(t, tc.wantApproved, approved)
			if tc.wantApply {
				assert.Len(t, *applies, 1, "scope applied")
			} else {
				assert.Empty(t, *applies, "no scope applied")
			}
			require.Len(t, *writes, 1, "exactly one cold_start_task write per outcome")
			assert.Equal(t, tc.wantStatus, (*writes)[0].content.Status)
		})
	}
}

// TestHost_AwaitDecision_ApproveCleanedWritesCleanedText verifies the cleaned
// text reaches the task on approve_cleaned (the runner places it as turn 0).
func TestHost_AwaitDecision_ApproveCleanedWritesCleanedText(t *testing.T) {
	orch := &fakeOrch{dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveCleaned}}
	h, _, writes, _ := newColdStartHost(t, orch)
	reqID, err := h.PublishApproval(context.Background(), coldStartAsk())
	require.NoError(t, err)
	_, _, _, err = h.AwaitDecision(context.Background(), reqID, time.Minute)
	require.NoError(t, err)
	require.Len(t, *writes, 1)
	assert.Equal(t, "summarize L-140", (*writes)[0].content.CleanedText)
}

// metaagentScopeAsk builds a mid-session metaagent_scope ApprovalAsk. handleOne
// passes no RequesterPerms, so the realistic delta the gate sees is a HardDeny.
func metaagentScopeAsk() pipeline.ApprovalAsk {
	return pipeline.ApprovalAsk{
		Kind:    "metaagent_scope",
		Summary: "scope review",
		Payload: map[string]any{
			"requester":       "user:alice",
			"verbatim":        "@metaagent do not read ENG",
			"approverSummary": "Deny linear search.",
			"skippedExplain":  "",
			"caveatExplain":   "",
			"applied":         scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
			"skipped":         []scope.SkippedItem{},
			"caveats":         []scope.CaveatItem{},
		},
	}
}

// newMetaagentHost builds a Host with only the deps the metaagent-scope path
// needs: it resolves the decision, so no WriteTask / ApplyScope (MetaagentApply
// owns the effects).
func newMetaagentHost(t *testing.T, orch *fakeOrch) *Host {
	t.Helper()
	return New(Deps{
		Session:      SessionRef{Namespace: "ns", Name: "a"},
		Orchestrator: orch,
		Publish: func(_ context.Context, payload []byte) error {
			orch.published = payload
			return orch.publishErr
		},
	})
}

// TestHost_PublishApproval_MetaagentScope_PayloadShape verifies the mid-session
// payload carries the characterized field set, NO coldStart/cleanedTask keys
// (so channelsd renders the 3-button block), and a metaagent-scope- reqID.
func TestHost_PublishApproval_MetaagentScope_PayloadShape(t *testing.T) {
	orch := &fakeOrch{dec: approval.Decision{Approved: true, ApproverID: "user:bob"}}
	h := newMetaagentHost(t, orch)
	reqID, err := h.PublishApproval(context.Background(), metaagentScopeAsk())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(reqID, "metaagent-scope-"),
		"mid-session reqID must use the metaagent-scope- prefix")
	assert.Equal(t, 0, orch.awaits, "PublishApproval must NOT await yet")

	// AwaitDecision sends the envelope via OnPublish.
	_, _, _, err = h.AwaitDecision(context.Background(), reqID, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, orch.published, "the envelope must be published inside Await's OnPublish")

	var p map[string]any
	require.NoError(t, json.Unmarshal(orch.published, &p))
	for _, key := range []string{
		"requestId", "requester", "verbatim",
		"approverSummary", "skippedExplain", "caveatExplain", "applied", "skipped", "caveats",
	} {
		_, ok := p[key]
		assert.Truef(t, ok, "mid-session payload must carry %q", key)
	}
	assert.Len(t, p, 9, "mid-session payload must carry EXACTLY the 9-field set (no extras)")
	_, hasColdStart := p["coldStart"]
	assert.False(t, hasColdStart, "mid-session payload must NOT carry coldStart (3-button render)")
	_, hasCleaned := p["cleanedTask"]
	assert.False(t, hasCleaned, "mid-session payload must NOT carry cleanedTask")
	assert.Equal(t, reqID, p["requestId"], "requestId must equal the minted reqID (envelope/await key match)")
	assert.Equal(t, "user:alice", p["requester"])
	assert.Equal(t, "@metaagent do not read ENG", p["verbatim"])
	assert.Equal(t, "Deny linear search.", p["approverSummary"])
}

// TestHost_AwaitDecision_MetaagentScope_ResolvesOnly verifies the mid-session
// path is a pure publish→await→bool resolver (5b): no apply, no WriteTask.
// approve→(true,approver,nil); deny→(false,approver,nil); awaitErr→(false,"",nil)
// (sticky no, no Halt). The orchestrator is always awaited and the envelope
// flows through OnPublish.
func TestHost_AwaitDecision_MetaagentScope_ResolvesOnly(t *testing.T) {
	cases := []struct {
		name         string
		dec          approval.Decision
		awaitErr     error
		wantApproved bool
		wantApprover string
	}{
		{
			name:         "approve → (true, approver, nil)",
			dec:          approval.Decision{Approved: true, ApproverID: "user:bob"},
			wantApproved: true, wantApprover: "user:bob",
		},
		{
			name:         "deny → (false, approver, nil)",
			dec:          approval.Decision{Approved: false, ApproverID: "user:bob"},
			wantApproved: false, wantApprover: "user:bob",
		},
		{
			name:         "await error → (false, \"\", nil): sticky no, no Halt",
			awaitErr:     errors.New("orch timeout"),
			wantApproved: false, wantApprover: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := &fakeOrch{dec: tc.dec, awaitErr: tc.awaitErr}
			h := newMetaagentHost(t, orch)
			reqID, err := h.PublishApproval(context.Background(), metaagentScopeAsk())
			require.NoError(t, err)
			approved, by, _, err := h.AwaitDecision(context.Background(), reqID, time.Minute)
			require.NoError(t, err, "await error maps to sticky-no (approved=false), NOT a host error")
			assert.Equal(t, tc.wantApproved, approved)
			assert.Equal(t, tc.wantApprover, by)
			assert.Equal(t, 1, orch.awaits, "the orchestrator is awaited exactly once")
			require.NotNil(t, orch.published, "the envelope flowed through OnPublish")
		})
	}
}

// TestHost_AwaitDecision_InterleavedKinds proves the two ApprovalAsk kinds do
// not collide: a cold-start and a metaagent-scope reqID resolve independently
// through the same Host (single tagged pending map, dispatch by stored kind).
func TestHost_AwaitDecision_InterleavedKinds(t *testing.T) {
	csOrch := &fakeOrch{dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveCleaned}}
	csHost, _, csWrites, _ := newColdStartHost(t, csOrch)
	csReqID, err := csHost.PublishApproval(context.Background(), coldStartAsk())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(csReqID, "metaagent-coldstart-"))

	msOrch := &fakeOrch{dec: approval.Decision{Approved: true, ApproverID: "user:bob"}}
	msHost := newMetaagentHost(t, msOrch)
	msReqID, err := msHost.PublishApproval(context.Background(), metaagentScopeAsk())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(msReqID, "metaagent-scope-"))
	assert.NotEqual(t, csReqID, msReqID)

	// Cold-start resolves with the five-way effect (one task write).
	csApproved, _, _, err := csHost.AwaitDecision(context.Background(), csReqID, time.Minute)
	require.NoError(t, err)
	assert.True(t, csApproved)
	assert.Len(t, *csWrites, 1, "cold-start writes a cold_start_task")

	// Metaagent-scope resolves to a pure bool (no task write).
	msApproved, _, _, err := msHost.AwaitDecision(context.Background(), msReqID, time.Minute)
	require.NoError(t, err)
	assert.True(t, msApproved)
}

// TestHost_Effects verifies Notify/Audit/SetStatus/Halt are best-effort.
func TestHost_Effects(t *testing.T) {
	published := []string{}
	h := New(Deps{
		Session: SessionRef{Namespace: "ns", Name: "a"},
		NoticePublish: func(_ context.Context, _, body string) error {
			published = append(published, body)
			return nil
		},
	})
	ctx := context.Background()
	assert.NoError(t, h.Notify(ctx, pipeline.Notice{Notice: testNotice("hello"), ToRequester: true}))
	assert.Equal(t, []string{"hello — Send it again."}, published, "Notify routes to the metaagent_notice publish")
	assert.NoError(t, h.SetStatus(ctx, pipeline.StatusUpdate{Text: "x"}))
	assert.NoError(t, h.Halt(ctx, "y"))
	assert.NoError(t, h.Audit(ctx, []pipeline.AuditRecord{{Kind: "approval_resolved"}}))
}

// --- ResolveApproval: effect-free staged resolver ---

// TestHost_ResolveApproval_ColdStart_NoEffect verifies the staged resolver
// publishes the byte-identical cold-start payload + resolves the 5-way action,
// but does NOT apply scope or write a task (MetaagentApply owns those effects).
func TestHost_ResolveApproval_ColdStart_NoEffect(t *testing.T) {
	cases := []struct {
		name         string
		dec          approval.Decision
		awaitErr     error
		wantApproved bool
		wantAction   string
	}{
		{name: "approve_cleaned → (true, approve_cleaned)", dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveCleaned}, wantApproved: true, wantAction: coldstart.ActionApproveCleaned},
		{name: "approve_original → (true, approve_original)", dec: approval.Decision{Approved: true, Action: coldstart.ActionApproveOriginal}, wantApproved: true, wantAction: coldstart.ActionApproveOriginal},
		{name: "run_without_scope → (false, run_without_scope)", dec: approval.Decision{Approved: true, Action: coldstart.ActionRunWithoutScope}, wantApproved: false, wantAction: coldstart.ActionRunWithoutScope},
		{name: "deny → (false, deny)", dec: approval.Decision{Approved: false, Action: coldstart.ActionDeny}, wantApproved: false, wantAction: coldstart.ActionDeny},
		{name: "await error → (false, deny)", awaitErr: errors.New("orch timeout"), wantApproved: false, wantAction: coldstart.ActionDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := &fakeOrch{dec: tc.dec, awaitErr: tc.awaitErr}
			h, applies, writes, _ := newColdStartHost(t, orch)
			approved, action, err := h.ResolveApproval(context.Background(), coldStartAsk(), time.Minute)
			require.NoError(t, err, "await error maps to deny, not a host error")
			assert.Equal(t, tc.wantApproved, approved)
			assert.Equal(t, tc.wantAction, action)
			assert.Empty(t, *applies, "ResolveApproval must NOT apply scope (Apply owns it)")
			assert.Empty(t, *writes, "ResolveApproval must NOT write a task (Apply owns it)")
			require.NotNil(t, orch.published, "the byte-identical envelope still flows through OnPublish")
			var p map[string]any
			require.NoError(t, json.Unmarshal(orch.published, &p))
			assert.Equal(t, true, p["coldStart"], "cold-start payload preserved")
		})
	}
}

// TestHost_ResolveApproval_MetaagentScope_NoEffect verifies the staged resolver
// returns a pure two-way bool (no action) for metaagent_scope, no effects, and
// the 3-button payload (no coldStart key).
func TestHost_ResolveApproval_MetaagentScope_NoEffect(t *testing.T) {
	orch := &fakeOrch{dec: approval.Decision{Approved: true, ApproverID: "user:bob"}}
	h := newMetaagentHost(t, orch)
	approved, action, err := h.ResolveApproval(context.Background(), metaagentScopeAsk(), time.Minute)
	require.NoError(t, err)
	assert.True(t, approved)
	assert.Equal(t, "", action, "metaagent_scope resolves a pure two-way bool")
	var p map[string]any
	require.NoError(t, json.Unmarshal(orch.published, &p))
	_, hasColdStart := p["coldStart"]
	assert.False(t, hasColdStart, "3-button payload preserved (no coldStart)")
}

// TestHost_Scratch_RoundTrips verifies the per-request scratch threads state
// across the four staged hooks (Received→Extract→Decide→Apply): each Set* is
// observable via the matching Peek* on the SAME Host, mutex-safe, and the
// distinct slots do not collide.
func TestHost_Scratch_RoundTrips(t *testing.T) {
	h := New(Deps{Session: SessionRef{Namespace: "ns", Name: "a"}})

	// Defaults before any Set: zero values.
	assert.Equal(t, "", h.PeekKind())
	delta, shape, cleaned := h.PeekExtract()
	assert.True(t, delta.IsEmpty())
	assert.Equal(t, "", shape)
	assert.Equal(t, "", cleaned)
	approved0, action0 := h.PeekApproved()
	assert.False(t, approved0)
	assert.Equal(t, "", action0)

	// Received stamps the kind.
	h.SetKind("cold_start")
	assert.Equal(t, "cold_start", h.PeekKind())

	// Extract writes delta/shape/cleaned.
	d := scope.ScopeDelta{Add: scope.ScopePartial{Tools: []string{"linear.search_issues"}}}
	h.SetExtract(d, "widen", "summarize L-140")
	gotDelta, gotShape, gotCleaned := h.PeekExtract()
	assert.Equal(t, d, gotDelta)
	assert.Equal(t, "widen", gotShape)
	assert.Equal(t, "summarize L-140", gotCleaned)

	// Decide writes the composed output + the resolved approval bool.
	out := scope.MetaagentOutput{Delta: d, ApproverSummary: "Add linear search."}
	h.SetDecide(out)
	assert.Equal(t, out, h.PeekDecide())
	h.SetApproved(true, "approve_cleaned")
	gotApproved, gotAction := h.PeekApproved()
	assert.True(t, gotApproved)
	assert.Equal(t, "approve_cleaned", gotAction)

	// The kind slot is untouched by later writes.
	assert.Equal(t, "cold_start", h.PeekKind())
}

// TestHost_Scratch_IsolatedPerHost verifies two Hosts (two concurrent
// requests) own independent scratch — one Host's Set does not leak into
// another's Peek.
func TestHost_Scratch_IsolatedPerHost(t *testing.T) {
	h1 := New(Deps{Session: SessionRef{Namespace: "ns", Name: "a"}})
	h2 := New(Deps{Session: SessionRef{Namespace: "ns", Name: "b"}})
	h1.SetKind("cold_start")
	h2.SetKind("mid_session")
	assert.Equal(t, "cold_start", h1.PeekKind())
	assert.Equal(t, "mid_session", h2.PeekKind())
}

var _ pipeline.Host = New(Deps{})

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
