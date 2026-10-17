package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/webactionapproval"
)

// recordingWebActionBinder is a RelWriter that records the grant relationships
// GrantSlots asks it to write (or fails them all when err is set).
type recordingWebActionBinder struct {
	wrote []authz.Relation
	err   error
}

func (r *recordingWebActionBinder) WriteRelationships(_ context.Context, rels []authz.Relation) error {
	if r.err != nil {
		return r.err
	}
	r.wrote = append(r.wrote, rels...)
	return nil
}

func (r *recordingWebActionBinder) DeleteRelationships(context.Context, []authz.Relation) error {
	return nil
}

func webActionTC() *pendingToolCall {
	return &pendingToolCall{
		sessNS:               "ns",
		sessName:             "s",
		toolName:             "browser_click",
		useID:                "tu-1",
		reqID:                "req-1",
		webActionKey:         "aa.com/click/place-order-abcd1234",
		webActionFingerprint: "fp-A",
	}
}

func webActionAuditScope() memory.Scope { return memory.Scope{Kind: "session", ID: "ns/s"} }

// persistWebActionApproval is the E9 approval-write effect: mint the
// fingerprint-caveated grant AND append the append-only audit record. Both halves
// are best-effort but neither is ever silent, and the two are INDEPENDENT — a
// grant that cannot be written must not take the audit down with it, and vice
// versa. Only webActionGrantBinding was unit-tested before; these cover the
// orchestration around it.

// TestPersistWebActionApproval_HappyPath mints the caveated grant and writes the
// audit record.
func TestPersistWebActionApproval_HappyPath(t *testing.T) {
	binder := &recordingWebActionBinder{}
	mem := memory.NewLocal(inmem.NewBackend())
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: binder, Mem: mem},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	h.persistWebActionApproval(context.Background(), webActionTC(), approval.Decision{Approved: true, ApproverID: "user:alice"})

	require.Len(t, binder.wrote, 1, "the caveated web_action grant must be minted")
	assert.Equal(t, "web_action", binder.wrote[0].ResourceType)

	recs, err := webactionapproval.List(memory.WithSystemApproval(context.Background(), "test"), mem, webActionAuditScope())
	require.NoError(t, err)
	require.Len(t, recs, 1, "the approval must be audited")
	assert.Equal(t, "approved", recs[0].Decision)
	assert.Equal(t, "user:alice", recs[0].Approver)
	assert.Equal(t, "fp-A", recs[0].ApprovedFingerprint)
}

// TestPersistWebActionApproval_EmptyKeyIsFullNoOp pins the webActionKey guard:
// persist is called on EVERY tool_call approve, so a non-browser approval
// (empty key) must touch neither the grant writer nor the audit log.
func TestPersistWebActionApproval_EmptyKeyIsFullNoOp(t *testing.T) {
	binder := &recordingWebActionBinder{}
	mem := memory.NewLocal(inmem.NewBackend())
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: binder, Mem: mem},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	tc := webActionTC()
	tc.webActionKey = ""
	h.persistWebActionApproval(context.Background(), tc, approval.Decision{Approved: true, ApproverID: "user:alice"})

	assert.Empty(t, binder.wrote, "an empty web_action key must mint no grant")
	recs, err := webactionapproval.List(memory.WithSystemApproval(context.Background(), "test"), mem, webActionAuditScope())
	require.NoError(t, err)
	assert.Empty(t, recs, "an empty web_action key must write no audit record")
}

// TestPersistWebActionApproval_EmptyFingerprintSkipsGrantButStillAudits: a mutate
// the tool could not fingerprint mints no grant (one would be permanently dead)
// but IS still audited, so the gap is itself evidence.
func TestPersistWebActionApproval_EmptyFingerprintSkipsGrantButStillAudits(t *testing.T) {
	binder := &recordingWebActionBinder{}
	mem := memory.NewLocal(inmem.NewBackend())
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: binder, Mem: mem},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	tc := webActionTC()
	tc.webActionFingerprint = ""
	h.persistWebActionApproval(context.Background(), tc, approval.Decision{Approved: true, ApproverID: "user:alice"})

	assert.Empty(t, binder.wrote, "an empty fingerprint must not mint a permanently-dead grant")
	recs, err := webactionapproval.List(memory.WithSystemApproval(context.Background(), "test"), mem, webActionAuditScope())
	require.NoError(t, err)
	require.Len(t, recs, 1, "the empty-fingerprint approval must still be audited")
	assert.Equal(t, "", recs[0].ApprovedFingerprint)
}

// TestPersistWebActionApproval_NilSlotBinderStillAudits: no grant writer wired
// (a test / kubectl-driven session) skips the grant loudly but still audits.
func TestPersistWebActionApproval_NilSlotBinderStillAudits(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: nil, Mem: mem},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	h.persistWebActionApproval(context.Background(), webActionTC(), approval.Decision{Approved: true, ApproverID: "user:alice"})

	recs, err := webactionapproval.List(memory.WithSystemApproval(context.Background(), "test"), mem, webActionAuditScope())
	require.NoError(t, err)
	require.Len(t, recs, 1, "a missing grant writer must not suppress the audit")
}

// TestPersistWebActionApproval_GrantFailureStillAudits: GrantSlots erroring must
// be logged and must NOT take the independent audit append down with it.
func TestPersistWebActionApproval_GrantFailureStillAudits(t *testing.T) {
	binder := &recordingWebActionBinder{err: errors.New("spicedb unavailable")}
	mem := memory.NewLocal(inmem.NewBackend())
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: binder, Mem: mem},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	h.persistWebActionApproval(context.Background(), webActionTC(), approval.Decision{Approved: true, ApproverID: "user:alice"})

	assert.Empty(t, binder.wrote, "the failing writer recorded nothing")
	recs, err := webactionapproval.List(memory.WithSystemApproval(context.Background(), "test"), mem, webActionAuditScope())
	require.NoError(t, err)
	require.Len(t, recs, 1, "a grant-write failure must not suppress the audit record")
}

// TestPersistWebActionApproval_NilMemLoudSkip: no memory store wired mints the
// grant but cannot audit — it must loud-skip, never panic on the nil Mem.
func TestPersistWebActionApproval_NilMemLoudSkip(t *testing.T) {
	binder := &recordingWebActionBinder{}
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: binder, Mem: nil},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	require.NotPanics(t, func() {
		h.persistWebActionApproval(context.Background(), webActionTC(), approval.Decision{Approved: true, ApproverID: "user:alice"})
	}, "a nil memory store must loud-skip the audit, not panic")
	require.Len(t, binder.wrote, 1, "the grant is still minted even when the audit cannot be written")
}

// TestPersistWebActionApproval_EmptyUseIDSkipsAudit: a missing tool-use id cannot
// key the for_tool_call link, so the audit is skipped (loudly) rather than
// written orphaned — but the grant is still minted.
func TestPersistWebActionApproval_EmptyUseIDSkipsAudit(t *testing.T) {
	binder := &recordingWebActionBinder{}
	mem := memory.NewLocal(inmem.NewBackend())
	h := newRunnerHost(&Loop{AgentName: "demo-agent", SlotBinder: binder, Mem: mem},
		hostSession{Namespace: "ns", Name: "s", Class: "demo-agent"})

	tc := webActionTC()
	tc.useID = ""
	h.persistWebActionApproval(context.Background(), tc, approval.Decision{Approved: true, ApproverID: "user:alice"})

	require.Len(t, binder.wrote, 1, "the grant is still minted")
	recs, err := webactionapproval.List(memory.WithSystemApproval(context.Background(), "test"), mem, webActionAuditScope())
	require.NoError(t, err)
	assert.Empty(t, recs, "a missing tool-use id must skip the audit rather than write it unpaired")
}
