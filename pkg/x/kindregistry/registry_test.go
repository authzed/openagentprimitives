package kindregistry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// item is a trivial registrable value: its key is its Name.
type item struct{ name string }

func newReg() *Registry[item] {
	return New[item]("testreg", func(i item) string { return i.name })
}

func TestRegisterGetAll(t *testing.T) {
	r := newReg()
	// Insert out of order; All must come back sorted by key.
	r.Register(item{name: "charlie"})
	r.Register(item{name: "alpha"})
	r.Register(item{name: "bravo"})

	got, ok := r.Get("alpha")
	require.True(t, ok, "Get(alpha) must hit")
	assert.Equal(t, "alpha", got.name)

	_, ok = r.Get("missing")
	assert.False(t, ok, "Get(missing) should miss")

	all := r.All()
	require.Len(t, all, 3)
	assert.Equal(t, []string{"alpha", "bravo", "charlie"},
		[]string{all[0].name, all[1].name, all[2].name}, "All must be sorted")

	assert.Equal(t, []string{"alpha", "bravo", "charlie"}, r.Keys(), "Keys must be sorted")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	r := newReg()
	r.Register(item{name: "dup"})
	assert.PanicsWithValue(t, `testreg: duplicate registration for "dup"`, func() {
		r.Register(item{name: "dup"})
	}, "duplicate key must panic with labeled message")
}

func TestRegisterEmptyKeyPanics(t *testing.T) {
	r := newReg()
	assert.PanicsWithValue(t, "testreg: registration with empty key", func() {
		r.Register(item{name: ""})
	}, "empty key must panic with labeled message")
}

func TestAllSnapshotIsolation(t *testing.T) {
	r := newReg()
	r.Register(item{name: "a"})
	r.Register(item{name: "b"})
	snap := r.All()
	require.Len(t, snap, 2)
	// Mutating the registry after All() must not affect the snapshot.
	r.Register(item{name: "c"})
	assert.Len(t, snap, 2, "All() snapshot must be independent of later writes")
}

func TestReset(t *testing.T) {
	r := newReg()
	r.Register(item{name: "x"})
	require.Len(t, r.All(), 1)
	r.Reset()
	assert.Empty(t, r.All(), "Reset must clear the registry")
	// Re-registering the same key after Reset must not panic.
	assert.NotPanics(t, func() { r.Register(item{name: "x"}) })
}
