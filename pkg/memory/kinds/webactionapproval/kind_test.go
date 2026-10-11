package webactionapproval_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/webactionapproval"
)

func TestWebActionApproval_Registered(t *testing.T) {
	k, ok := memory.LookupKind("web_action_approval")
	require.True(t, ok, "web_action_approval must be registered via its init()")
	assert.Equal(t, "webact-", k.IDPrefix())
	assert.True(t, k.Retention().AppendOnly, "web_action approvals are write-once audit evidence")
	assert.Equal(t, memory.SessionWritten, k.WriteAuthority(),
		"the runner records its own acting approvals on the per-session bearer")
}

func TestWebActionApproval_AppendRoundTrips(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	rec := webactionapproval.Record{
		WebActionKey:        "aa.com/click/place-order-abcd1234",
		ApprovedFingerprint: "fp-A",
		Decision:            "approved",
		Approver:            "user:alice",
		ToolName:            "browser_click",
		At:                  time.Unix(1700000000, 0).UTC(),
	}
	require.NoError(t, webactionapproval.Append(ctx, m, scope, "tu-1", "req-1", rec), "Append")

	got, err := webactionapproval.List(ctx, m, scope)
	require.NoError(t, err, "List")
	require.Len(t, got, 1)
	assert.Equal(t, "aa.com/click/place-order-abcd1234", got[0].WebActionKey)
	assert.Equal(t, "fp-A", got[0].ApprovedFingerprint)
	assert.Equal(t, "approved", got[0].Decision)
	assert.Equal(t, "user:alice", got[0].Approver)
}

// A byte-identical re-put of the SAME approval EVENT (same tool-use id AND
// the same ask's reqID) is idempotent; a re-put that CHANGES the content for
// that same event is refused — the append-only guarantee that makes this
// record tamper-evident audit evidence.
func TestWebActionApproval_AppendOnly_ChangingReputRefused(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	rec := webactionapproval.Record{
		WebActionKey:        "aa.com/click/place-order-abcd1234",
		ApprovedFingerprint: "fp-A",
		Decision:            "approved",
		Approver:            "user:alice",
		At:                  time.Unix(1700000000, 0).UTC(),
	}
	require.NoError(t, webactionapproval.Append(ctx, m, scope, "tu-1", "req-1", rec), "first Append")

	// Idempotent: the identical record (same (tool-use id, reqID) → same derived
	// id, same bytes) re-puts cleanly.
	require.NoError(t, webactionapproval.Append(ctx, m, scope, "tu-1", "req-1", rec),
		"a byte-identical re-put must be idempotent")

	// Tamper: the SAME approval event (same tool-use id, same reqID), DIFFERENT
	// approved fingerprint — a rewrite of what was approved against. Must be
	// refused.
	tampered := rec
	tampered.ApprovedFingerprint = "fp-FORGED"
	err := webactionapproval.Append(ctx, m, scope, "tu-1", "req-1", tampered)
	require.Error(t, err, "a changing re-put of an append-only approval must be refused")
	assert.ErrorIs(t, err, memory.ErrAppendOnlyConflict)

	// And the original is untouched.
	got, err := webactionapproval.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "fp-A", got[0].ApprovedFingerprint, "the original record must survive a tamper attempt")
}

// TWO DIFFERENT approval events sharing the SAME tool-use id (the scripted
// e2e test harness's single-tool-call-per-turn replies all mint "toolu_0",
// since each reply's tool_use index restarts at zero — see
// test/e2e/scripted_llm.go) must NOT collide: each carries its own reqID (the
// ask's own request id), so both persist as distinct records. This is the
// property Task E11's browser-consequence-gate bundle depends on: a SECOND
// distinct mutate, or the SAME mutate re-approved after the page changed
// (the TOCTOU re-prompt), must not silently lose its audit record to an
// ErrAppendOnlyConflict against an unrelated approval that happened to share
// a tool-use id.
func TestWebActionApproval_SameToolUseID_DifferentReqID_BothPersist(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	first := webactionapproval.Record{
		WebActionKey:        "aa.com/click/confirm-order-1111",
		ApprovedFingerprint: "fp-A",
		Decision:            "approved",
		At:                  time.Unix(1700000000, 0).UTC(),
	}
	second := webactionapproval.Record{
		WebActionKey:        "aa.com/click/cancel-subscription-2222",
		ApprovedFingerprint: "fp-B",
		Decision:            "approved",
		At:                  time.Unix(1700000001, 0).UTC(),
	}
	require.NoError(t, webactionapproval.Append(ctx, m, scope, "toolu_0", "req-1", first))
	require.NoError(t, webactionapproval.Append(ctx, m, scope, "toolu_0", "req-2", second),
		"a different reqID under the same tool-use id must be a distinct record, not a conflict")

	got, err := webactionapproval.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, got, 2, "both approval events must have persisted")
	keys := []string{got[0].WebActionKey, got[1].WebActionKey}
	assert.ElementsMatch(t, []string{"aa.com/click/confirm-order-1111", "aa.com/click/cancel-subscription-2222"}, keys)
}

// The empty-fingerprint degenerate case is still auditable: the write path mints
// no grant for it, but the approval record carries the empty fingerprint so the
// gap is itself evidence.
func TestWebActionApproval_RecordsEmptyFingerprint(t *testing.T) {
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "ns/a"}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	rec := webactionapproval.Record{
		WebActionKey:        "aa.com/submit/checkout-ffff0000",
		ApprovedFingerprint: "",
		Decision:            "approved",
		At:                  time.Unix(1700000001, 0).UTC(),
	}
	require.NoError(t, webactionapproval.Append(ctx, m, scope, "tu-2", "req-3", rec))

	got, err := webactionapproval.List(ctx, m, scope)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].ApprovedFingerprint)

	// Round-trips through JSON cleanly (the content is what an auditor decodes).
	raw, err := json.Marshal(got[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "checkout-ffff0000")
}
