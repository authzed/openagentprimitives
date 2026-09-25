// descriptor.go — runner-side aliases to the shared, kind-agnostic
// credential resolver. The per-kind descriptor builders that once lived
// here (CLICredentialDescriptors, MCPCredentialDescriptor) were removed:
// the runtime now emits authkind.CredentialRequirement per Kind and maps
// them onto descriptors through credresolve.Descriptors — the single,
// kind-agnostic path. The RuntimeIdentity aliases below remain so the
// runner callers (internal/cmd/runner, cmd/oap, e2e factory) keep a stable name.
package runner

import (
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

// RuntimeIdentity and its constructors moved to pkg/platform/identity/credresolve as
// part of the shared kind-agnostic resolver redesign. These aliases keep
// existing runner callers (internal/cmd/runner, cmd/oap, e2e factory) compiling.
type RuntimeIdentity = credresolve.RuntimeIdentity

var (
	RuntimeIdentityFromAgentIdentity       = credresolve.RuntimeIdentityFromAgentIdentity
	RuntimeIdentityFromSessionUserIdentity = credresolve.RuntimeIdentityFromSessionUserIdentity
)
