// Package registry is the process-wide registry of content-guard inspector
// kinds. Kinds self-register from init() in pkg/authz/contentguard/kinds/<name>/;
// binaries opt in with a blank import (same shape as pkg/authz/pinning/registry).
package registry

import (
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[contentguard.Inspector]("contentguard registry", contentguard.Inspector.ID)

// Register adds an inspector. Duplicate IDs panic (init-time programmer error).
func Register(i contentguard.Inspector) { reg.Register(i) }

// Get returns the inspector registered under id.
func Get(id string) (contentguard.Inspector, bool) { return reg.Get(id) }

// All returns all registered inspectors, sorted by id.
func All() []contentguard.Inspector { return reg.All() }

// Reset clears the registry. Test-only.
func Reset() { reg.Reset() }
