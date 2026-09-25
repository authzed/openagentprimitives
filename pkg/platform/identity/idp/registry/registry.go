package registry

import (
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// reg is the process-wide idp-kind registry, keyed by Kind.Name().
var reg = kindregistry.New[idp.Kind]("idp", idp.Kind.Name)

func Register(k idp.Kind) { reg.Register(k) }

func Get(name string) (idp.Kind, bool) { return reg.Get(name) }

// Names returns the names of all registered kinds, sorted.
func Names() []string { return reg.Keys() }

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }
