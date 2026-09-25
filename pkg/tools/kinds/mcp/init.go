// Package mcp registers the MCPServer Kind with the global tools-kind
// registry. Importing this package side-effects the registration; nothing
// else needs to be referenced.
package mcp

import "github.com/authzed/openagentprimitives/pkg/tools/kinds/registry"

func init() { registry.Register(New()) }
