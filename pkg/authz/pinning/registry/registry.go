// Package registry is the process-wide registry of pinning kinds. Kinds
// self-register from init() in pkg/authz/pinning/kinds/<name>/; binaries opt in
// with a blank import (same shape as pkg/channels/channelkinds/registry). Storage and
// the panic-on-dup/empty semantics live in pkg/x/kindregistry.
package registry

import (
	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide pinning-kind registry, keyed by Kind.Name().
var reg = kindregistry.New[pinning.Kind]("pinning registry", pinning.Kind.Name)

// Register adds a kind. Duplicate names panic: registration happens at init()
// time, so a duplicate is a programmer error, not a runtime condition.
func Register(k pinning.Kind) { reg.Register(k) }

// Get returns the kind registered under name.
func Get(name string) (pinning.Kind, bool) { return reg.Get(name) }

// All returns all registered kinds, sorted by name.
func All() []pinning.Kind { return reg.All() }

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }
