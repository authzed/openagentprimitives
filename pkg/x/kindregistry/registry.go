// Package kindregistry is the shared implementation behind the project's many
// string-keyed "kind" registries (channel kinds, artifact renderers, tool
// kinds, authkinds, idp kinds, pinning kinds, web UIs, toolkit-stream
// factories, …). Each keeps its own narrow exported API (Register/Get/All/
// Reset plus per-domain extras) as thin forwarders onto a package-private
// *Registry[T] built here.
//
// Registration is mutex-guarded, and an empty or duplicate key panics —
// programmer errors that must surface at init() rather than become
// recoverable at runtime.
package kindregistry

import (
	"fmt"
	"sort"
	"sync"
)

// Registry is a concurrency-safe map from a string key to a value of type T.
// The key is derived from each value via keyOf. label is used in panic
// messages so an operator (or a failing test) can tell which registry tripped.
type Registry[T any] struct {
	mu    sync.RWMutex
	label string
	keyOf func(T) string
	items map[string]T
}

// New constructs an empty Registry. label prefixes panic messages
// (e.g. "channelkinds"); keyOf extracts the registration key from a value
// (e.g. channelkinds.Kind.Name).
func New[T any](label string, keyOf func(T) string) *Registry[T] {
	return &Registry[T]{
		label: label,
		keyOf: keyOf,
		items: map[string]T{},
	}
}

// Register adds v under keyOf(v), panicking on an empty or duplicate key.
func (r *Registry[T]) Register(v T) {
	key := r.keyOf(v)
	r.mu.Lock()
	defer r.mu.Unlock()
	if key == "" {
		panic(fmt.Sprintf("%s: registration with empty key", r.label))
	}
	if _, exists := r.items[key]; exists {
		panic(fmt.Sprintf("%s: duplicate registration for %q", r.label, key))
	}
	r.items[key] = v
}

// Get returns the value registered under key, and whether one was found.
func (r *Registry[T]) Get(key string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[key]
	return v, ok
}

// All returns a snapshot of every registered value, sorted by key. The
// returned slice is independent of the registry's internal map.
func (r *Registry[T]) All() []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.items))
	for k := range r.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.items[k])
	}
	return out
}

// Keys returns every registered key, sorted. Convenience for registries that
// expose a names-only view.
func (r *Registry[T]) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.items))
	for k := range r.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func (r *Registry[T]) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = map[string]T{}
}
