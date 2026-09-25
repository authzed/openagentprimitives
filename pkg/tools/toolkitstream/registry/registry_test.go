package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/registry"
)

type stubFactory struct{ kind string }

func (s stubFactory) Kind() string            { return s.kind }
func (stubFactory) New() toolkitstream.Parser { return nil }

func TestRegistry_RegisterAndLookup(t *testing.T) {
	t.Cleanup(registry.Reset)
	registry.Register(stubFactory{kind: "alpha"})
	registry.Register(stubFactory{kind: "beta"})

	got, ok := registry.ByKind("alpha")
	require.True(t, ok)
	assert.Equal(t, "alpha", got.Kind())

	_, ok = registry.ByKind("unknown")
	assert.False(t, ok)
}

func TestRegistry_EmptyKindPanics(t *testing.T) {
	t.Cleanup(registry.Reset)
	assert.Panics(t, func() { registry.Register(stubFactory{}) })
}

func TestRegistry_DuplicateKindPanics(t *testing.T) {
	t.Cleanup(registry.Reset)
	registry.Register(stubFactory{kind: "dup"})
	assert.Panics(t, func() { registry.Register(stubFactory{kind: "dup"}) })
}
