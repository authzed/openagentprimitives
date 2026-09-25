// Package registry is the process-local registry of modality.Modality
// implementations.
//
// Modalities register themselves via init():
//
//	func init() { registry.Register(&myModality{}) }
//
// Consumers (e.g. the artifacts capability) call registry.All() to enumerate
// every registered modality rather than branching on which one is active.
package registry

import (
	"fmt"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/modality"
)

var (
	mu       sync.RWMutex
	registry = map[string]modality.Modality{}
)

// Register adds m. Panics on name collision so build/init ordering bugs
// are loud rather than silent.
func Register(m modality.Modality) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := registry[m.Name()]; exists {
		panic(fmt.Sprintf("modality.Register: duplicate name %q", m.Name()))
	}
	registry[m.Name()] = m
}

// All returns every registered Modality, sorted by Name for determinism.
func All() []modality.Modality {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]modality.Modality, 0, len(registry))
	for _, m := range registry {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// SnapshotForTest returns a copy of the current registry contents. Pair
// it with RestoreForTest (via t.Cleanup) so a test can mutate the
// global registry and still leave it exactly as init() seeded it. Tests
// only.
func SnapshotForTest() map[string]modality.Modality {
	mu.RLock()
	defer mu.RUnlock()
	snap := make(map[string]modality.Modality, len(registry))
	for k, v := range registry {
		snap[k] = v
	}
	return snap
}

// RestoreForTest replaces the registry with a snapshot previously
// obtained from SnapshotForTest. Tests only.
func RestoreForTest(snap map[string]modality.Modality) {
	mu.Lock()
	defer mu.Unlock()
	registry = make(map[string]modality.Modality, len(snap))
	for k, v := range snap {
		registry[k] = v
	}
}
