// Package registry is the process-wide sandbox-kind registry. The exported
// funcs are thin forwarders so the storage, mutexing, sorting and
// panic-on-duplicate live in pkg/x/kindregistry — the same shape channelkinds,
// artifact renderers and tool kinds use.
package registry

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[sandboxkinds.Kind]("sandboxkinds", sandboxkinds.Kind.Name)

// Register adds a kind. Panics on a duplicate or empty key — both are
// programmer errors detectable at init() time.
func Register(k sandboxkinds.Kind) { reg.Register(k) }

// Get returns the named kind. An unknown OR empty name misses: lookup is
// fail-closed, so a typo'd spec.sandbox.kind surfaces as a validation error
// rather than silently installing the built-in backend.
func Get(name string) (sandboxkinds.Kind, bool) { return reg.Get(name) }

// All returns every registered kind, sorted by name.
func All() []sandboxkinds.Kind { return reg.All() }

// Keys returns every registered kind name, sorted.
func Keys() []string { return reg.Keys() }

// MustGet is Get for callers that cannot proceed without the kind — init()
// and tests. Production lookup paths use Get and report the miss.
func MustGet(name string) sandboxkinds.Kind {
	k, ok := reg.Get(name)
	if !ok {
		panic(fmt.Sprintf("sandboxkinds: no kind registered for %q", name))
	}
	return k
}

// Reset clears the registry. Test-only helper; do not call from production
// code paths, and do not call it from a test that relies on init()-time
// registrations — nothing re-runs them.
func Reset() { reg.Reset() }
