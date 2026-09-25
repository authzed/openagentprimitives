package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/registry"
)

// stubBackend is a minimal Backend used only to exercise registry plumbing.
type stubBackend struct{ name string }

func (s stubBackend) Name() string                                   { return s.name }
func (s stubBackend) New(websearch.Deps) (websearch.Provider, error) { return nil, nil }

// registerOnce guards against re-registering when the package's tests run
// more than once in a single process. Never call registry.Reset() on the
// process-global registry: it would wipe init()-time registrations that
// nothing re-runs.
var registered bool

func ensureStub(t *testing.T, name string) {
	t.Helper()
	if !registered {
		registry.Register(stubBackend{name: name})
		registered = true
	}
}

func TestGet_HitAndMiss(t *testing.T) {
	ensureStub(t, "stub")

	got, ok := registry.Get("stub")
	require.True(t, ok, "Get(stub) must hit after Register")
	assert.Equal(t, "stub", got.Name())

	_, ok = registry.Get("nope")
	assert.False(t, ok, "Get of an unregistered name must miss")
}

// Lookup is fail-closed: an empty name is a miss, never a silent fallback to
// a default backend. A typo'd or unset backend name must surface as a
// configuration error rather than quietly selecting one.
func TestGet_EmptyNameMisses(t *testing.T) {
	ensureStub(t, "stub")

	_, ok := registry.Get("")
	assert.False(t, ok, "empty name must miss, not fall back")
}

func TestMustGet_PanicsOnUnknown(t *testing.T) {
	ensureStub(t, "stub")

	assert.PanicsWithValue(t, `websearch: no backend registered for "nope"`, func() {
		registry.MustGet("nope")
	})
}
