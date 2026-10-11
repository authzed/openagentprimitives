package directcred

import "github.com/authzed/openagentprimitives/pkg/x/kindregistry"

// reg is the process-wide directcred registry, keyed by Source.Kind().
// Storage/mutex/sort/panic-on-dup live in pkg/x/kindregistry; the exported
// funcs below are thin forwarders, same shape as pkg/tools/kinds/registry.
var reg = kindregistry.New[Source]("directcred", Source.Kind)

// Register adds s to the registry. Panics on duplicate Kind().
func Register(s Source) { reg.Register(s) }

// Get returns the source registered under kind.
func Get(kind string) (Source, bool) { return reg.Get(kind) }

// All returns a snapshot of every registered source, sorted by Kind().
func All() []Source { return reg.All() }

// Reset clears the registry. Test-only helper; do not call from
// production code paths.
func Reset() { reg.Reset() }
