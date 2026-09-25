package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

// stubKind is a minimal Kind used only to exercise registry plumbing.
type stubKind struct{ name string }

func (s stubKind) Name() string                                   { return s.name }
func (s stubKind) Supports(sandboxkinds.Feature) bool             { return false }
func (s stubKind) WorkspaceDomain() string                        { return "" }
func (s stubKind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }
func (s stubKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, nil
}

// registerOnce guards against re-registering when the package's tests run more
// than once in a single process. Never call registry.Reset() on the
// process-global registry: it would wipe init()-time registrations that
// nothing re-runs.
var registered bool

func ensureStub(t *testing.T, name string) {
	t.Helper()
	if !registered {
		registry.Register(stubKind{name: name})
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

// Lookup is fail-closed: an empty kind is a miss, never a silent fallback to
// the built-in backend. A typo'd or unset spec.sandbox.kind must surface as a
// validation error rather than quietly installing the default substrate.
func TestGet_EmptyNameMisses(t *testing.T) {
	ensureStub(t, "stub")

	_, ok := registry.Get("")
	assert.False(t, ok, "empty kind must miss, not fall back")
}

func TestMustGet_PanicsOnUnknown(t *testing.T) {
	ensureStub(t, "stub")

	assert.PanicsWithValue(t, `sandboxkinds: no kind registered for "nope"`, func() {
		registry.MustGet("nope")
	})
}
