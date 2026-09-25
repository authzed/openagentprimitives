// Package registry is the process-local registry of toolchain delivery Kinds.
// Kinds self-register via init(); binaries blank-import the ones they support.
package registry

import (
	"fmt"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/tools/toolchain"
)

var (
	mu       sync.RWMutex
	registry = map[string]toolchain.Kind{}
)

// Register adds k. Panics on name collision so an init-ordering bug is loud at
// startup rather than a silently-shadowed delivery mechanism at pod-create.
func Register(k toolchain.Kind) {
	mu.Lock()
	defer mu.Unlock()
	if _, exists := registry[k.Name()]; exists {
		panic(fmt.Sprintf("toolchain registry: duplicate kind %q", k.Name()))
	}
	registry[k.Name()] = k
}

// ByKind returns the Kind registered under name.
func ByKind(name string) (toolchain.Kind, bool) {
	mu.RLock()
	defer mu.RUnlock()
	k, ok := registry[name]
	return k, ok
}

// Names returns every registered kind name, sorted. Used to build actionable
// "unknown kind %q (known: %v)" errors.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
