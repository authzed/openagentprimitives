// Package registry is the global lookup for agent harnesses. Backends
// self-register via an init(); consumers resolve by name
// (registry-over-branching). Mirrors pkg/platform/workspacekinds/registry and
// pkg/channels/channelkinds/registry.
package registry

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[harness.Harness]("harness", harness.Harness.Name)

// Register adds a harness. Panics on empty name or duplicate — both are
// programmer errors caught at process start, not runtime conditions.
func Register(h harness.Harness) { reg.Register(h) }

// Get looks up a harness by its Name().
func Get(name string) (harness.Harness, bool) { return reg.Get(name) }

// All returns every registered harness, sorted by name.
func All() []harness.Harness { return reg.All() }

// Reset clears the registry (test-only).
func Reset() { reg.Reset() }

// Resolve maps an AgentClass.spec.harness value to a harness, fail-closed.
// An ABSENT (empty) name resolves to harness.DefaultName; an unregistered
// name is an error, never a silent downgrade — a typo'd spec.harness must
// surface rather than quietly running the built-in loop.
//
// Returns a nil interface (not a typed nil) alongside any error.
func Resolve(name string) (harness.Harness, error) {
	if name == "" {
		name = harness.DefaultName
	}
	h, ok := reg.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (registered: %v)", name, reg.Keys())
	}
	return h, nil
}
