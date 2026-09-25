// Package sidecartoolbox registers the SidecarToolbox Kind with the
// global tools-kind registry. Importing this package side-effects
// the registration.
package sidecartoolbox

import "github.com/authzed/openagentprimitives/pkg/tools/kinds/registry"

func init() { registry.Register(New()) }
