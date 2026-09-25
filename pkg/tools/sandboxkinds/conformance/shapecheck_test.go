package conformance_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/conformance"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/internal/shapecheck"
)

// The off-cluster backend runs the same contract as the Kubernetes ones, with
// no cluster in sight.
func TestConformance_Shapecheck(t *testing.T) {
	rt, err := shapecheck.Kind{}.NewRuntime(sandboxkinds.Deps{})
	require.NoError(t, err)

	conformance.Run(t, conformance.Subject{
		Name:    "shapecheck",
		Runtime: rt,
		NewRequest: func() sandboxkinds.EnsureRequest {
			return sandboxkinds.EnsureRequest{
				Session: shapecheck.DemoSession(),
				Class:   shapecheck.DemoClass(),
			}
		},
		// An off-cluster provider hands back a running sandbox; there is no
		// scheduler to wait on.
		ExpectReadyAfterEnsure: true,
	})
}
