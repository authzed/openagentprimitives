package completion

import (
	"fmt"
	"sort"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = map[string]Requirement{}
)

// Register adds r. Panics on key collision so build/init ordering bugs are
// loud rather than silent, matching every other kind registry in the repo.
func Register(r Requirement) {
	mu.Lock()
	defer mu.Unlock()
	if r.Key() == KeyCheckFailed {
		panic(fmt.Sprintf("completion.Register: %q is reserved for the synthetic check-failed entry", KeyCheckFailed))
	}
	if _, exists := registry[r.Key()]; exists {
		panic(fmt.Sprintf("completion.Register: duplicate requirement key %q", r.Key()))
	}
	registry[r.Key()] = r
}

// Get resolves a declared key to its registered Requirement.
//
// This is THE lookup: Evaluate routes every declared key through it, so every
// registered kind reaches its Check by the same path. That is deliberate — a
// consumer that resolved kinds any other way would be a switch statement
// wearing a registry's clothes.
func Get(key string) (Requirement, bool) {
	mu.RLock()
	defer mu.RUnlock()
	r, ok := registry[key]
	return r, ok
}

// Keys returns every registered key, sorted. Used to name the legal values in
// an error a human or a model reads.
func Keys() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SnapshotForTest returns a copy of the current registry contents. Pair it with
// RestoreForTest (via t.Cleanup) so a test can mutate the global registry and
// still leave it exactly as init() seeded it. Tests only.
func SnapshotForTest() map[string]Requirement {
	mu.RLock()
	defer mu.RUnlock()
	snap := make(map[string]Requirement, len(registry))
	for k, v := range registry {
		snap[k] = v
	}
	return snap
}

// RestoreForTest replaces the registry with a snapshot previously obtained from
// SnapshotForTest. Tests only.
func RestoreForTest(snap map[string]Requirement) {
	mu.Lock()
	defer mu.Unlock()
	registry = make(map[string]Requirement, len(snap))
	for k, v := range snap {
		registry[k] = v
	}
}
