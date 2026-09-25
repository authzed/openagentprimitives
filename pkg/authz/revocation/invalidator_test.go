package revocation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInv records the keys it was asked to invalidate. err, when set, is
// returned from every Invalidate — the shape a real Invalidator reports when
// it reached only some (or none) of a credential's holders
// (kinds/credential.Invalidator joins its targets' failures).
// noun is left empty by most cases on purpose: the interface documents an empty
// noun as legal, so the fake must be able to produce one.
type fakeInv struct {
	kind string
	noun string
	err  error
	got  []string
}

func (f *fakeInv) Kind() string                { return f.kind }
func (f *fakeInv) Noun() string                { return f.noun }
func (f *fakeInv) Invalidate(key string) error { f.got = append(f.got, key); return f.err }

func TestRegistryDispatchesByKind(t *testing.T) {
	r := NewRegistry()
	a := &fakeInv{kind: "tool-origin"}
	require.NoError(t, r.Register(a))

	inv, ok := r.Lookup("tool-origin")
	require.True(t, ok)
	require.NoError(t, inv.Invalidate("mcpserver/x"))
	assert.Equal(t, []string{"mcpserver/x"}, a.got)

	_, ok = r.Lookup("unknown")
	assert.False(t, ok, "unknown kind must not resolve")
}

func TestRegistryRejectsDuplicateKind(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(&fakeInv{kind: "credential"}))
	assert.Error(t, r.Register(&fakeInv{kind: "credential"}), "duplicate kind must error")
}

func TestRegistryRejectsEmptyKind(t *testing.T) {
	assert.Error(t, NewRegistry().Register(&fakeInv{kind: ""}), "empty kind must error")
}
