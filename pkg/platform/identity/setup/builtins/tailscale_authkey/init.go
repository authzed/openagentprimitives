package tailscale_authkey

import "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"

func init() { builtins.Register(New()) }
