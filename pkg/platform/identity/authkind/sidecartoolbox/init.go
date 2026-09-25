package sidecartoolbox

import "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/registry"

func init() { registry.Register(New()) }
