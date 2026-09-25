package audit

import (
	"fmt"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Mapper converts entries of one memory kind into normalized audit events.
// A Map returning (nil, nil) means "this entry contributes no events"
// (e.g. a turn with no tool_use blocks) — not an error.
type Mapper interface {
	MemoryKind() string
	EventKinds() []string // every Event.Kind this mapper can emit
	Map(e memory.Entry) ([]Event, error)
}

var (
	regMu   sync.RWMutex
	mappers = map[string]Mapper{}
)

// Register adds m. Panics on duplicate memory kind (mirrors the other
// registries in this repo — registration bugs should fail at init).
func Register(m Mapper) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := mappers[m.MemoryKind()]; exists {
		panic(fmt.Sprintf("audit: duplicate mapper for memory kind %q", m.MemoryKind()))
	}
	mappers[m.MemoryKind()] = m
}

// MapperFor returns the mapper registered for a memory kind.
func MapperFor(memoryKind string) (Mapper, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	m, ok := mappers[memoryKind]
	return m, ok
}

// MemoryKinds returns every registered memory kind, sorted.
func MemoryKinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(mappers))
	for k := range mappers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MemoryKindsForEventKinds returns the memory kinds whose mappers can emit
// any of the requested event kinds; empty input = all registered kinds.
func MemoryKindsForEventKinds(eventKinds []string) []string {
	if len(eventKinds) == 0 {
		return MemoryKinds()
	}
	want := map[string]bool{}
	for _, k := range eventKinds {
		want[k] = true
	}
	regMu.RLock()
	defer regMu.RUnlock()
	var out []string
	for mk, m := range mappers {
		for _, ek := range m.EventKinds() {
			if want[ek] {
				out = append(out, mk)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
