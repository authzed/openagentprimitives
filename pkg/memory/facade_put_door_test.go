package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// TestPutDoor_Mutable exercises the WriteMemory door on a mutable Kind:
// no approval denies, a matching WriteMemory (or system) approval allows,
// and an approval for the wrong verb (ReadMemory) still denies.
func TestPutDoor_Mutable(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(fakeKind{name: "mutable-door", prefix: "md-"})
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	e := memory.Entry{
		Scope:     scope,
		Kind:      "mutable-door",
		ID:        "md-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}

	// No approval: denied before ever touching the backend.
	_, err := m.Put(context.Background(), e)
	require.ErrorIs(t, err, memory.ErrMissingApproval)

	// Matching WriteMemory approval: allowed.
	ctxWrite := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.WriteMemory, scope.ID, "tok-1"))
	_, err = m.Put(ctxWrite, e)
	assert.NoError(t, err)

	// System approval: allowed.
	_, err = m.Put(memory.WithSystemApproval(context.Background(), "operator:test"), e)
	assert.NoError(t, err)

	// ReadMemory approval (wrong verb): still denied.
	ctxRead := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, scope.ID, "tok-2"))
	_, err = m.Put(ctxRead, e)
	require.ErrorIs(t, err, memory.ErrMissingApproval)
}

// TestPutDoor_AppendOnlySelfMintsWithoutVerifier proves that append-only
// writes keep working with no external approval and no configured
// ProvenanceVerifier: the facade self-mints the internal AppendAudit
// approval after the (skipped) verify step, so tamper-evident writes are
// never accidentally locked out by the new WriteMemory door.
func TestPutDoor_AppendOnlySelfMintsWithoutVerifier(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao-door", prefix: "aod-"})
	m := memory.NewLocal(inmem.NewBackend()) // no WithProvenanceVerifier

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "nsA/sessA"},
		Kind:      "ao-door",
		ID:        "aod-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}

	// No provVerify configured AND no external approval of any kind: still
	// succeeds. A WriteMemory or system approval is neither necessary nor
	// sufficient here — self-minting is.
	_, err := m.Put(context.Background(), e)
	assert.NoError(t, err)
}

// TestPutDoor_AppendOnlyVerifyErrorIsTheGate proves the append-only door has
// teeth: a failing ProvenanceVerifier's error surfaces from Put verbatim and
// backend.Put is never reached, even when the caller carries a WriteMemory
// approval — provenance verification is the gate, not a caller-supplied
// capability.
func TestPutDoor_AppendOnlyVerifyErrorIsTheGate(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(appendOnlyKind{name: "ao-door", prefix: "aod-"})
	verifyErr := errors.New("signature invalid")
	v := &fakeVerifier{returns: verifyErr}
	b := &fakeBackend{}
	m := memory.NewLocal(b, memory.WithProvenanceVerifier(v))

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "nsA/sessA"},
		Kind:      "ao-door",
		ID:        "aod-1",
		CreatedAt: time.Unix(0, 0).UTC(),
		Content:   json.RawMessage(`{"v":1}`),
	}

	// A caller-supplied WriteMemory approval does NOT satisfy the
	// append-only door.
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.WriteMemory, e.Scope.ID, "tok-1"))
	_, err := m.Put(ctx, e)
	require.ErrorIs(t, err, verifyErr)
	assert.Empty(t, b.puts, "backend.Put must not run when verify rejects")
}
