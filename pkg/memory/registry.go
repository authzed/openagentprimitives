package memory

import (
	"fmt"
	"strings"
	"sync"
)

var (
	regMu    sync.RWMutex
	byName   = map[string]Kind{}
	byPrefix = map[string]Kind{}
)

// RegisterKind adds k to the global Kind registry, called from each Kind
// package's init() so collisions panic at startup rather than at first Put.
// Panics on:
//   - empty Name() or empty IDPrefix();
//   - a Name() starting with "_" (reserved for HTTP scope routes like "_query");
//   - duplicate Name();
//   - exact IDPrefix() collision with another Kind;
//   - any substring-prefix overlap with another Kind's prefix, so "op-" cannot
//     coexist with "ops-" — an ID would otherwise validate under both.
func RegisterKind(k Kind) {
	if k.Name() == "" {
		panic("memory.RegisterKind: empty Name()")
	}
	if strings.HasPrefix(k.Name(), "_") {
		panic(fmt.Sprintf(
			`memory.RegisterKind: Kind name %q must not start with "_" (reserved for HTTP scope routes)`,
			k.Name()))
	}
	if k.IDPrefix() == "" {
		panic(fmt.Sprintf("memory.RegisterKind: Kind %q has empty IDPrefix()", k.Name()))
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := byName[k.Name()]; exists {
		panic(fmt.Sprintf("memory.RegisterKind: duplicate Kind name %q", k.Name()))
	}
	if other, exists := byPrefix[k.IDPrefix()]; exists {
		panic(fmt.Sprintf("memory.RegisterKind: Kind %q IDPrefix %q collides with Kind %q",
			k.Name(), k.IDPrefix(), other.Name()))
	}
	for p, other := range byPrefix {
		if strings.HasPrefix(k.IDPrefix(), p) || strings.HasPrefix(p, k.IDPrefix()) {
			panic(fmt.Sprintf("memory.RegisterKind: Kind %q IDPrefix %q overlaps with Kind %q prefix %q",
				k.Name(), k.IDPrefix(), other.Name(), p))
		}
	}
	byName[k.Name()] = k
	byPrefix[k.IDPrefix()] = k
}

// LookupKind returns the Kind registered under name, for Put validation and for
// callers holding a Kind name as a string.
func LookupKind(name string) (Kind, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	k, ok := byName[name]
	return k, ok
}

// RegisteredKinds snapshots every registered Kind, sorted by Name so ScopeHooks
// materialization and signal dispatch are deterministic.
func RegisteredKinds() []Kind {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Kind, 0, len(byName))
	for _, k := range byName {
		out = append(out, k)
	}
	sortKindsByName(out)
	return out
}

// ResetRegistryForTest clears the registry. Tests only.
func ResetRegistryForTest() {
	regMu.Lock()
	defer regMu.Unlock()
	byName = map[string]Kind{}
	byPrefix = map[string]Kind{}
}

// KindAppendOnly reports whether the named registered Kind is append-only.
// Unknown kinds read as mutable: append-only enforcement only ever tightens
// behavior for kinds that explicitly asked for it.
func KindAppendOnly(name string) bool {
	k, ok := LookupKind(name)
	return ok && k.Retention().AppendOnly
}

func sortKindsByName(ks []Kind) {
	// Insertion sort: N is the number of registered Kinds (tens). Switch to
	// slices.SortFunc if it ever grows enough to matter.
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j-1].Name() > ks[j].Name(); j-- {
			ks[j-1], ks[j] = ks[j], ks[j-1]
		}
	}
}
