// Package meta holds the process-local meta tools — the tools the agent calls
// against its own session rather than against the outside world.
//
// Most of them are NOT in the registry below. A capability constructs the tools
// it offers by calling their exported New… constructor, so which meta tools a
// session gets is decided by its capabilities.
//
// Register/Load cover only the always-on terminal tools that every session has
// regardless of capability — agent_work_complete and new_operation. Those two
// register in init() and are collected by the core capability:
//
//	func init() { meta.Register(&myTool{}) }
//
// Add a tool here only if it must be present unconditionally; anything
// capability-gated gets a constructor instead.
package meta

import (
	"fmt"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

var (
	mu       sync.RWMutex
	registry = map[string]tool.Tool{}
)

// Register adds t. Panics on name collision so build/init ordering bugs
// are loud rather than silent.
func Register(t tool.Tool) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := registry[t.Name()]; exists {
		panic(fmt.Sprintf("meta.Register: duplicate tool name %q", t.Name()))
	}
	registry[t.Name()] = t
}

// Load returns every registered Tool, sorted by Name for determinism.
func Load() []tool.Tool {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]tool.Tool, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// SnapshotForTest returns a copy of the current registry contents. Pair
// it with RestoreForTest (via t.Cleanup) so a test can mutate the
// global registry and still leave it exactly as init() seeded it. Tests
// only.
func SnapshotForTest() map[string]tool.Tool {
	mu.RLock()
	defer mu.RUnlock()
	snap := make(map[string]tool.Tool, len(registry))
	for k, v := range registry {
		snap[k] = v
	}
	return snap
}

// RestoreForTest replaces the registry with a snapshot previously
// obtained from SnapshotForTest. Tests only.
func RestoreForTest(snap map[string]tool.Tool) {
	mu.Lock()
	defer mu.Unlock()
	registry = make(map[string]tool.Tool, len(snap))
	for k, v := range snap {
		registry[k] = v
	}
}

// LookupForTest returns the tool registered under name, or nil if not
// present. Tests only.
func LookupForTest(name string) tool.Tool {
	mu.RLock()
	defer mu.RUnlock()
	return registry[name]
}
