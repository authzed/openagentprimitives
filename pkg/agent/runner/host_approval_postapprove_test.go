package runner

import (
	"context"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	memapproval "github.com/authzed/openagentprimitives/pkg/memory/kinds/approval"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// gatedToolCallAsk is a tool_call ask whose StateImpact is Readonly with a
// Check — unlike toolCallAsk's External, which SKIPS the post-approval
// re-check entirely. This is the shape that actually runs
// toolCallPostApprove.
func gatedToolCallAsk() pipeline.ApprovalAsk {
	payload := ToolCallApprovalPayload{
		SessNS:       "ns",
		SessName:     "s",
		ToolName:     "list_contacts",
		Permission:   "contact_access",
		ResourceType: "crm_company",
		ResourceID:   "acme-id",
		StateImpact:  "readonly",
		ArgsHash:     "h1",
		ArgsJSON:     `{"companyId":"acme-id"}`,
		Perm: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "crm_company",
				ResourceIDTemplate: "acme-id",
				Permission:         "contact_access",
			},
		},
		UseID: "tu-postapprove",
	}
	return pipeline.ApprovalAsk{
		Kind:    "tool_call",
		Summary: "read contacts on acme-id",
		Payload: payload.ToMap(),
	}
}

// TestAwaitDecision_PostApproveDenial_RecordsDeniedOutcome pins the audit
// ledger to what HAPPENED rather than to what was clicked.
//
// The human approves, the post-approval authorization re-check then denies,
// and AwaitDecision returns approved=false — but it used to return there
// without recording any outcome, leaving the append-only, signed approval log
// holding a request with no verdict while the lifecycle log's
// DecisionResolved said Approved=true. An auditor reading that pair sees an
// allowed call that was never allowed.
//
// Here the re-check denies because no SpiceDB client is wired (authz's
// fail-closed Cli==nil branch), which is the same Result.IsError() path a
// genuine schema-semantics denial takes.
func TestAwaitDecision_PostApproveDenial_RecordsDeniedOutcome(t *testing.T) {
	orch := approval.New()
	mem := memory.NewLocal(inmem.NewBackend())
	l := &Loop{
		ResourceStandings: testResourceStandings(),
		Status:            LocalStatusPatcher(),
		Approval:          orch,
		ChannelKind:       "slack",
		Mem:               mem,
		// AuthzCli deliberately nil: the post-approve re-check must fail.
		SpiceDBLookupSubjects: func(_ context.Context, _, _ string) ([]string, error) {
			return []string{"user:owner@corp.example"}, nil
		},
		InteractionRequestPublish: func(_ context.Context, _, _ string, _ channelevents.Envelope) error {
			return nil
		},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	reqID, err := h.PublishApproval(context.Background(), gatedToolCallAsk())
	require.NoError(t, err, "PublishApproval")

	go func() {
		time.Sleep(10 * time.Millisecond)
		orch.DeliverDecision(reqID, approval.Decision{Approved: true, ApproverID: "alice"})
	}()

	approved, by, timedOut, err := h.AwaitDecision(context.Background(), reqID, 5*time.Second)
	require.NoError(t, err, "AwaitDecision must not error; the denial is a verdict")
	assert.False(t, approved, "a post-approve re-check denial must surface as approved=false")
	assert.False(t, timedOut, "this is a denial, not a timeout")
	assert.Equal(t, "alice", by, "the approver identity is still reported")

	pair, err := memapproval.ByToolCall(memory.WithSystemApproval(context.Background(), "test"), mem,
		memory.Scope{Kind: "session", ID: "ns/s"}, "tu-postapprove")
	require.NoError(t, err, "ByToolCall")
	require.NotNil(t, pair.Outcome,
		"the ledger must carry an outcome: a request with no verdict alongside DecisionResolved{Approved:true} reads as an allowed call")
	assert.Equal(t, "denied", pair.Outcome.Decision,
		"the recorded decision must be what happened to the CALL, not what the approver clicked")
	assert.Equal(t, "alice", pair.Outcome.Approver)
	assert.Contains(t, pair.Outcome.Reason, "post-approval authorization re-check denied",
		"the reason must name the re-check so an auditor can tell this from an approver's own denial")
}

// recordingSlotRevoker implements authz.RelWriter, capturing
// DeleteRelationships calls on a buffered channel so the one-shot-revoke
// goroutine toolCallPostApprove launches (host_approval.go ~:1884) can be
// awaited deterministically instead of raced with a sleep.
type recordingSlotRevoker struct {
	deletes chan []authz.Relation
}

func newRecordingSlotRevoker() *recordingSlotRevoker {
	return &recordingSlotRevoker{deletes: make(chan []authz.Relation, 1)}
}

func (r *recordingSlotRevoker) WriteRelationships(_ context.Context, _ []authz.Relation) error {
	return nil
}

func (r *recordingSlotRevoker) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	r.deletes <- rels
	return nil
}

// noopGuardianGrantWriter satisfies grants.Writer. toolCallPostApprove's
// one-shot-revoke branch only nil-checks Loop.GuardianGrantWriter to decide
// whether to run at all (host_approval.go ~:1860) — it never calls a method
// on it in this path, the actual revoke goes through Loop.SlotRevoker — so a
// writer that satisfies the interface with no-ops is sufficient to exercise
// the gate.
type noopGuardianGrantWriter struct{}

func (noopGuardianGrantWriter) WriteRelationships(_ context.Context, _ *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	return &v1.WriteRelationshipsResponse{}, nil
}

func (noopGuardianGrantWriter) DeleteRelationships(_ context.Context, _ *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	return &v1.DeleteRelationshipsResponse{}, nil
}

// TestToolCallPostApprove_WebActionKeySkipsOneShotRevoke pins the E11 fix for
// a real production bug: toolCallPostApprove's one-shot cleanup used to
// revoke the slot grant named by tc.perm.Check unconditionally for every
// StateImpact:External approval. tc.perm is always the acting tool's OWN
// Permission() — web_host#act for every browser acting op
// (pkg/agent/tool/browser/tools.go) — even when the approval that actually
// fired was the nested per-action web_action layer (Phase E's E8/E9 gate,
// checkExternalSlotGrant). Approving one mutate therefore revoked the
// STANDING, plan-gate-granted web_host slot that is meant to cover the whole
// acting phase from one approval
// (browser-login-secret-source: "ONE APPROVAL, MANY ACTING CALLS") — the very
// next acting call on the same host would have no grant left and re-prompt.
//
// tc.webActionKey != "" is the signal that THIS card's authorization gap was
// the per-action web_action layer, not the coarse web_host one, so the guard
// at host_approval.go ~:1860 must skip the one-shot revoke entirely. This
// test fails (a revoke is observed) if that guard regresses.
func TestToolCallPostApprove_WebActionKeySkipsOneShotRevoke(t *testing.T) {
	revoker := newRecordingSlotRevoker()
	l := &Loop{
		Status:              LocalStatusPatcher(),
		GuardianGrantWriter: noopGuardianGrantWriter{},
		SlotRevoker:         revoker,
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	tc := &pendingToolCall{
		sessNS:   "ns",
		sessName: "s",
		toolName: "web_click",
		argsMap:  map[string]any{"host": "shop.example"},
		argsHash: "hash",
		// webActionKey set: this card's gap was the per-action web_action
		// layer, so the standing web_host grant must survive the cleanup.
		webActionKey:         "shop.example/click/confirm-seat-14c-abcd1234",
		webActionFingerprint: "fp-abc",
		perm: authz.Permission{
			StateImpact: authz.External, // tc.perm is always the acting tool's own Permission()
			Check: &authz.PermissionCheck{
				ResourceType:       "web_host",
				Permission:         "act",
				ResourceIDTemplate: "shop.example",
			},
		},
	}

	err := h.toolCallPostApprove(context.Background(), tc)
	require.NoError(t, err, "toolCallPostApprove")

	select {
	case rels := <-revoker.deletes:
		t.Fatalf("a web_action mutate approval must NOT revoke the standing web_host grant, got DeleteRelationships(%+v)", rels)
	case <-time.After(300 * time.Millisecond):
		// Expected: no revoke scheduled.
	}
}

// recordingSlotBinder implements authz.RelWriter, capturing
// WriteRelationships calls on a buffered channel so the standing-upgrade
// goroutine toolCallPostApprove launches for a DECLARED slot
// (upgradeDeclaredSlotGrantToStanding) can be awaited deterministically.
type recordingSlotBinder struct {
	writes chan []authz.Relation
}

func newRecordingSlotBinder() *recordingSlotBinder {
	return &recordingSlotBinder{writes: make(chan []authz.Relation, 1)}
}

func (b *recordingSlotBinder) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	b.writes <- rels
	return nil
}

func (b *recordingSlotBinder) DeleteRelationships(_ context.Context, _ []authz.Relation) error {
	return nil
}

// TestToolCallPostApprove_DeclaredSlotSkipsRevoke_AndBecomesStanding pins the
// task-46 fix: a JIT approval (planGate:disabled, webActionKey=="") of a
// DECLARED slot type (resourceType ∈ h.l.PlanGateSlotTypes, e.g. web_host)
// must NOT be one-shot-revoked the way an undeclared External tool is — it
// must instead be upgraded to a STANDING grant (session-length expiry), the
// same "one approval, many acting calls" property the enforcing/plan-gate
// path already gives narrowToApproved's grant. Before this fix, the
// condition at host_approval.go ~:1887 did not check slot-declared-ness at
// all, so every browser acting approval (web_host is always External) was
// revoked the instant its one call ran, and the very next acting call
// re-prompted — the bug this test guards against regressing.
func TestToolCallPostApprove_DeclaredSlotSkipsRevoke_AndBecomesStanding(t *testing.T) {
	revoker := newRecordingSlotRevoker()
	binder := newRecordingSlotBinder()
	mem := memory.NewLocal(inmem.NewBackend())
	l := &Loop{
		Status:              LocalStatusPatcher(),
		GuardianGrantWriter: noopGuardianGrantWriter{},
		SlotRevoker:         revoker,
		SlotBinder:          binder,
		Mem:                 mem,
		PlanGateSlotTypes:   []string{"web_host"},
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	tc := &pendingToolCall{
		sessNS:   "ns",
		sessName: "s",
		toolName: "web_type",
		argsMap:  map[string]any{"host": "shop.example"},
		argsHash: "hash",
		// webActionKey left empty: this is the coarse web_host JIT ask, not a
		// web_action mutate re-check.
		perm: authz.Permission{
			StateImpact: authz.External,
			Check: &authz.PermissionCheck{
				ResourceType:       "web_host",
				Permission:         "act",
				ResourceIDTemplate: "shop.example",
			},
		},
	}

	err := h.toolCallPostApprove(context.Background(), tc)
	require.NoError(t, err, "toolCallPostApprove")

	select {
	case rels := <-revoker.deletes:
		t.Fatalf("a declared slot type must NOT be one-shot-revoked, got DeleteRelationships(%+v)", rels)
	case <-time.After(300 * time.Millisecond):
		// Expected: no revoke scheduled.
	}

	select {
	case rels := <-binder.writes:
		require.Len(t, rels, 1, "exactly one slot grant is (re)written")
		assert.Equal(t, "web_host", rels[0].ResourceType)
		assert.Equal(t, "shop.example", rels[0].ResourceID)
		assert.Equal(t, "ns/s", rels[0].SubjectID)
		assert.True(t, rels[0].ExpiresAt.After(time.Now().Add(1*time.Minute)),
			"the upgraded grant must outlive External's flat 30s TTL — that is what makes it standing")
	case <-time.After(2 * time.Second):
		t.Fatal("a declared slot's JIT approval must be upgraded to a standing grant")
	}
}

// TestToolCallPostApprove_NoWebActionKeyStillRevokesOneShot is the
// pre-existing-behavior half of the same guard: a genuine one-off,
// undeclared-slot External tool approved per-call (webActionKey == "", the
// case the one-shot cleanup exists for) must still have its slot grant
// revoked the moment the one call it authorized has run — the E11 guard must
// change nothing for this shape.
func TestToolCallPostApprove_NoWebActionKeyStillRevokesOneShot(t *testing.T) {
	revoker := newRecordingSlotRevoker()
	l := &Loop{
		Status:              LocalStatusPatcher(),
		GuardianGrantWriter: noopGuardianGrantWriter{},
		SlotRevoker:         revoker,
	}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	tc := &pendingToolCall{
		sessNS:   "ns",
		sessName: "s",
		toolName: "apply_workspace",
		argsMap:  map[string]any{"op": "git_push"},
		argsHash: "hash",
		// webActionKey left empty: a genuine one-off External approval.
		perm: authz.Permission{
			StateImpact: authz.External,
			Check: &authz.PermissionCheck{
				ResourceType:       "github_repo",
				Permission:         "write",
				ResourceIDTemplate: "acme/repo",
			},
		},
	}

	err := h.toolCallPostApprove(context.Background(), tc)
	require.NoError(t, err, "toolCallPostApprove")

	select {
	case rels := <-revoker.deletes:
		require.Len(t, rels, 1, "exactly one slot grant is revoked")
		assert.Equal(t, "github_repo", rels[0].ResourceType)
		assert.Equal(t, "acme/repo", rels[0].ResourceID)
		assert.Equal(t, "ns/s", rels[0].SubjectID)
	case <-time.After(2 * time.Second):
		t.Fatal("a one-off External approval must still revoke its one-shot slot grant")
	}
}
