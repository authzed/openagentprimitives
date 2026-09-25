// Package mcpui — registry init.
package mcpui

import "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"

func init() { registry.Register(New()) }
