// Package registry is the process-wide web-search-backend registry. The
// exported funcs are thin forwarders so the storage, mutexing, sorting and
// panic-on-duplicate live in pkg/x/kindregistry — the same shape channelkinds,
// sandboxkinds and artifact renderers use.
package registry

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[websearch.Backend]("websearch", websearch.Backend.Name)

// Register adds a backend. Panics on a duplicate or empty key — both are
// programmer errors detectable at init() time.
func Register(b websearch.Backend) { reg.Register(b) }

// Get returns the named backend. An unknown OR empty name misses: lookup is
// fail-closed, so a typo'd backend name surfaces as a configuration error
// rather than silently falling back to some default.
func Get(name string) (websearch.Backend, bool) { return reg.Get(name) }

// All returns every registered backend, sorted by name.
func All() []websearch.Backend { return reg.All() }

// Keys returns every registered backend name, sorted.
func Keys() []string { return reg.Keys() }

// MustGet is Get for callers that cannot proceed without the backend —
// init() and tests. Production lookup paths use Get and report the miss.
func MustGet(name string) websearch.Backend {
	b, ok := reg.Get(name)
	if !ok {
		panic(fmt.Sprintf("websearch: no backend registered for %q", name))
	}
	return b
}

// Reset clears the registry. Test-only helper; do not call from production
// code paths, and do not call it from a test that relies on init()-time
// registrations — nothing re-runs them.
func Reset() { reg.Reset() }
