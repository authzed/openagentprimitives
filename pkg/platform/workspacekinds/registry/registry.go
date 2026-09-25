// Package registry is the global lookup for workspace-source drivers. Backends
// self-register via an init(); consumers resolve a driver by name
// (registry-over-branching). Mirrors pkg/channels/channelkinds/registry and
// pkg/tools/kinds/registry.
package registry

import (
	"github.com/authzed/openagentprimitives/pkg/platform/workspacekinds"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[workspacekinds.Kind]("workspacekinds", workspacekinds.Kind.Name)

// Register adds a driver. Panics on empty name or duplicate (mirrors the other
// kind registries).
func Register(k workspacekinds.Kind) { reg.Register(k) }

// Get looks up a driver by its Name().
func Get(name string) (workspacekinds.Kind, bool) { return reg.Get(name) }

// All returns every registered driver, sorted by name.
func All() []workspacekinds.Kind { return reg.All() }

// Reset clears the registry (test-only).
func Reset() { reg.Reset() }
