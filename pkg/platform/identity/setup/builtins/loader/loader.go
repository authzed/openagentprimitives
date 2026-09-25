// Package loader blank-imports every v1 builtin flow so binaries get
// all of them registered with a single import.
package loader

import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/anthropic_oauth"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/kubectl_kubeconfig"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/oauth_mcp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/onepassword_scim"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/slack_bot_token"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/tailscale_authkey"
)
