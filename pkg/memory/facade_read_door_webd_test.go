package memory_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// The per-kind read door asks "may this credential see this Kind", and it
// decided by asking a DIFFERENT question: whether the context carries a token
// SESSION. That mark exists for provenance — it binds an append-only entry's
// publisher to its writer — and the webd token, which acts on any session and
// belongs to no session, never gets one. So the door skipped webd entirely and
// it could read pt_tag_content, and every other platform-only Kind, for any
// session in the cluster.
//
// webd is the one browser-facing holder of a component token. Its own branch in
// the memory server already says so in prose — read-only, session-scoped
// approval rather than the wildcard its siblings carry — and this is the same
// judgement, applied to the Kind axis.
//
// The door now takes an explicit mark, so being subject to it is something a
// caller is TOLD it is rather than something inferred from an unrelated
// mechanism.

// webdCtx is the browser-facing component token: session-scoped approval, no
// token session (it belongs to no session), and explicitly behind the door.
func webdCtx() context.Context {
	ctx := memory.WithApproval(context.Background(),
		memory.ForBearerToken(memory.ReadMemory, readDoorScope, "tok-webd"))
	return memory.WithKindReadDoor(ctx, "webd")
}

func TestQuery_KindReadDoor_AppliesToTheWebdToken(t *testing.T) {
	m := newReadDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope}

	_, err := m.Query(webdCtx(), memory.Query{Scope: scope, Kinds: []string{hiddenKind}})

	require.Error(t, err, "a browser-facing token naming a platform-only Kind must be refused")
	assert.ErrorIs(t, err, memory.ErrKindNotSessionReadable)
}

// The silent half too: a query that narrows on nothing legitimately sweeps the
// scope, and the platform-only entries must be dropped from what webd gets back
// rather than returned because no Kind was named.
func TestQuery_KindReadDoor_FiltersUnnamedKindsForWebd(t *testing.T) {
	m := newReadDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope} // newReadDoorStore seeds both Kinds

	got, err := m.Query(webdCtx(), memory.Query{Scope: scope})
	require.NoError(t, err)

	for _, e := range got.Entries {
		assert.NotEqual(t, hiddenKind, e.Kind,
			"a platform-only entry must not ride back on an unnarrowed webd query")
	}
}

// An in-process platform caller is unaffected — the door is for credentials
// that reached the process from outside, not for the operator reading its own
// records.
func TestQuery_KindReadDoor_StillIgnoresInProcessCallers(t *testing.T) {
	m := newReadDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope}

	_, err := m.Query(platformCtx(), memory.Query{Scope: scope, Kinds: []string{hiddenKind}})
	assert.NoError(t, err, "the operator reads its own platform-only Kinds")
}

// A per-session token stays behind the door on the mark it already had, so the
// new trigger is additive rather than a replacement — dropping the old one
// would have quietly reopened the door for every runner.
func TestQuery_KindReadDoor_StillAppliesToAPerSessionToken(t *testing.T) {
	m := newReadDoorStore(t)
	scope := memory.Scope{Kind: "session", ID: readDoorScope}

	_, err := m.Query(sessionCtx(), memory.Query{Scope: scope, Kinds: []string{hiddenKind}})
	assert.ErrorIs(t, err, memory.ErrKindNotSessionReadable)
}
