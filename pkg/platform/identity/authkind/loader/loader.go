// Package loader blank-imports every authkind impl so a binary gets all of them
// registered with a single import. New kinds are added here.
package loader

import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/sidecartoolbox"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/toolspec"
)
