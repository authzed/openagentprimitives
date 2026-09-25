package github

import "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

func init() { registry.Register(&Kind{}) }
