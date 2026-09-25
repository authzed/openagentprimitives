// Package image — registry init.
package image

import "github.com/authzed/openagentprimitives/pkg/channels/channelassets/registry"

func init() { registry.Register(New()) }
