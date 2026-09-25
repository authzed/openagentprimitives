package capability

import (
	"fmt"
	"sort"
	"sync"
)

var (
	regMu sync.RWMutex
	reg   = map[string]Capability{}
)

// Register adds a capability. Panics on duplicate name (a programming error).
func Register(c Capability) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[c.Name()]; dup {
		panic(fmt.Sprintf("capability: duplicate registration for %q", c.Name()))
	}
	reg[c.Name()] = c
}

// Lookup returns the registered capability by name.
func Lookup(name string) (Capability, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	c, ok := reg[name]
	return c, ok
}

// Ordered returns all capabilities in a deterministic assembly order:
// infra core first, introspection last, everything else alphabetical between.
func Ordered() []Capability {
	regMu.RLock()
	out := make([]Capability, 0, len(reg))
	for _, c := range reg {
		out = append(out, c)
	}
	regMu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		return orderKey(out[i]) < orderKey(out[j])
	})
	return out
}

func orderKey(c Capability) string {
	switch {
	case c.Name() == "introspection":
		return "z_introspection" // always last
	case c.Infrastructural():
		return "0_" + c.Name() // infra (core) before regular
	default:
		return "1_" + c.Name()
	}
}

// resetRegistryForTest clears the registry and restores it after the test.
func resetRegistryForTest(t interface{ Cleanup(func()) }) {
	regMu.Lock()
	prev := reg
	reg = map[string]Capability{}
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		reg = prev
		regMu.Unlock()
	})
}
