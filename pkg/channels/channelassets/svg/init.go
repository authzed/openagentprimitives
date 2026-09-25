// Package svg — registry init.
package svg

import "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"

func init() { registry.Register(New()) }
