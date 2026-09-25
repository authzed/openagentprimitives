// Package registry is the process-wide agent-UI component registry. The
// exported funcs are thin forwarders onto pkg/x/kindregistry, matching every
// other kind registry in the project (channelkinds, artifact renderers, tool
// kinds, …).
//
// There is exactly one source of registrations today: pkg/web/uicomponents's own
// init(). Bundle-supplied components are deferred; when they
// land they become an additional CALLER of Register, not a refactor of this
// package.
package registry

import (
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents/component"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

var reg = kindregistry.New[component.Component]("uicomponents", component.Component.Key)

func Register(c component.Component) { reg.Register(c) }

func Get(t string) (component.Component, bool) { return reg.Get(t) }

func All() []component.Component { return reg.All() }

func Keys() []string { return reg.Keys() }

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }
