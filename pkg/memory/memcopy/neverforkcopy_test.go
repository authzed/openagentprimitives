package memcopy_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/memcopy"
)

// chainKind stands in for a Kind whose entries are a CHAIN — order and the
// per-(scope, publisher) provenance sequence carry meaning, so copying them
// into a new scope would produce a history the child does not actually have.
type chainKind struct{ never bool }

func (chainKind) Name() string                          { return "chain_kind" }
func (chainKind) IDPrefix() string                      { return "chain-" }
func (chainKind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (k chainKind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true, NeverForkCopy: k.never}
}
func (chainKind) ContentSchema() reflect.Type                  { return nil }
func (chainKind) IndexedFields() []string                      { return nil }
func (chainKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopScopeHooks{} }

type noopScopeHooks struct{}

func (noopScopeHooks) OnSignal(context.Context, memory.Signal) error { return nil }

func seedChain(t *testing.T, mem memory.Memory, scope memory.Scope) {
	t.Helper()
	ctx := memory.WithSystemApproval(context.Background(), "test")
	for i, body := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		_, err := mem.Put(ctx, memory.Entry{
			Scope:     scope,
			Kind:      "chain_kind",
			ID:        "chain-" + string(rune('a'+i)),
			CreatedAt: time.Unix(int64(i), 0).UTC(),
			Content:   json.RawMessage(body),
		})
		require.NoError(t, err)
	}
}

func countKind(t *testing.T, mem memory.Memory, scope memory.Scope, kind string) int {
	t.Helper()
	res, err := mem.Query(memory.WithSystemApproval(context.Background(), "test"),
		memory.Query{Scope: scope})
	require.NoError(t, err)
	n := 0
	for _, e := range res.Entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// A Kind marked NeverForkCopy must not reach the child at all. The forking
// code derives what the child needs instead.
func TestCopyPrefix_neverForkCopyKindIsNotCopied(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(chainKind{never: true})

	mem := memory.NewLocal(inmem.NewBackend())
	src := memory.Scope{Kind: "session", ID: "ns/parent"}
	dst := memory.Scope{Kind: "session", ID: "ns/child"}
	seedChain(t, mem, src)

	require.NoError(t, memcopy.CopyPrefix(
		memory.WithSystemApproval(context.Background(), "test"), mem, src, dst, 99))

	assert.Equal(t, 3, countKind(t, mem, src, "chain_kind"), "the parent keeps its chain intact")
	assert.Zero(t, countKind(t, mem, dst, "chain_kind"),
		"a chain-shaped Kind must not be copied into a scope that has no such history")
}

// The default is unchanged: a Kind that does not opt in is still copied, so
// this is additive and no existing Kind changes behavior.
func TestCopyPrefix_ordinaryKindIsStillCopied(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(chainKind{never: false})

	mem := memory.NewLocal(inmem.NewBackend())
	src := memory.Scope{Kind: "session", ID: "ns/parent"}
	dst := memory.Scope{Kind: "session", ID: "ns/child"}
	seedChain(t, mem, src)

	require.NoError(t, memcopy.CopyPrefix(
		memory.WithSystemApproval(context.Background(), "test"), mem, src, dst, 99))

	assert.Equal(t, 3, countKind(t, mem, dst, "chain_kind"),
		"opting out is per-Kind; everything else copies as before")
}
