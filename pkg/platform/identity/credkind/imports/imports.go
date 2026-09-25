// Package imports blank-imports every credkind so a binary gets the full
// registry with a single import. Binaries that resolve or validate
// credentials import this package for side effects.
package imports

import (
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/federated"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/githubapp"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/oauth"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/static"
)
