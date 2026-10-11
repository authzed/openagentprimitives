package secretsource

import "github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"

func init() { directcred.Register(New()) }
