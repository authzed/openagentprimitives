// Package registry is the process-wide binding-source resolver registry. The
// exported funcs are thin forwarders onto pkg/x/kindregistry, matching every
// other kind registry in the project (channelkinds, artifact renderers, tool
// kinds, pkg/web/uicomponents/registry, …).
//
// Each binding-source package registers itself via Register from its own
// init(), so Keys() is the live set — do not transcribe it into a comment. A
// consumer of this package must never grow a source-name switch of its own.
package registry

import (
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[uibindings.Resolver]("uibindings", uibindings.Resolver.Source)

func Register(r uibindings.Resolver) { reg.Register(r) }

func Get(source string) (uibindings.Resolver, bool) { return reg.Get(source) }

func Keys() []string { return reg.Keys() }

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }
