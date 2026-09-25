// Package css — registry init.
package css

import "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"

func init() { registry.Register(New()) }
