package claude

import "github.com/authzed/openagentprimitives/pkg/tools/toolkitstream/registry"

func init() { registry.Register(&Factory{}) }
