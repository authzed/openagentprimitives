// Package registry holds the global parser-factory registry. Each
// parser under pkg/tools/toolkitstream/<x>/ calls Register from an init()
// so a blank-import in internal/cmd/runner is enough to wire it in. Storage and
// the panic-on-empty/dup semantics live in pkg/x/kindregistry.
package registry

import (
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide toolkit-stream factory registry, keyed by
// Factory.Kind().
var reg = kindregistry.New[toolkitstream.Factory]("toolkitstream", toolkitstream.Factory.Kind)

// Register adds f to the registry. Panics on empty Kind() or
// duplicate registration — both are programmer errors caught at
// process start.
func Register(f toolkitstream.Factory) { reg.Register(f) }

// ByKind returns the factory registered under kind, if any.
func ByKind(kind string) (toolkitstream.Factory, bool) { return reg.Get(kind) }

// Reset clears the registry. Test-only; never call from production code.
func Reset() { reg.Reset() }
