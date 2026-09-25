package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds/registry"
)

// fakeKind is a minimal in-test driver used to exercise the registry without
// depending on any real backend.
type fakeKind struct{ name string }

func (f fakeKind) Name() string                       { return f.name }
func (f fakeKind) Validate(workspacekinds.Spec) error { return nil }
func (f fakeKind) MaterializeCommands(workspacekinds.Spec, string) ([]workspacekinds.Command, error) {
	return nil, nil
}
func (f fakeKind) SyncCommands(workspacekinds.Spec, string) ([]workspacekinds.Command, error) {
	return nil, nil
}

func TestRegistry_RegisterGetAllSorted(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)

	registry.Register(fakeKind{name: "beta"})
	registry.Register(fakeKind{name: "alpha"})

	got, ok := registry.Get("alpha")
	require.True(t, ok, "alpha must be registered")
	assert.Equal(t, "alpha", got.Name())

	_, ok = registry.Get("missing")
	assert.False(t, ok, "unknown kind must report not-found")

	all := registry.All()
	require.Len(t, all, 2)
	assert.Equal(t, "alpha", all[0].Name(), "All() sorted by name")
	assert.Equal(t, "beta", all[1].Name())
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)
	registry.Register(fakeKind{name: "git"})
	assert.Panics(t, func() { registry.Register(fakeKind{name: "git"}) },
		"duplicate registration must panic")
}
