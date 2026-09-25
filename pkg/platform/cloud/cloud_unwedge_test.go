package cloud_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
)

func TestUnwedgeTerminatingNamespace_NoOpClouds(t *testing.T) {
	// eks/aks/local have no cloud-specific finalizer wedge: an empty report, no error.
	for _, s := range []cloud.Strategy{eks.Strategy{}, aks.Strategy{}, local.Strategy{}} {
		t.Run(s.DisplayName()+": empty report, no error", func(t *testing.T) {
			rep, err := s.UnwedgeTerminatingNamespace(context.Background(), cloud.Clients{}, cloud.NopReporter{}, "agentprimitives-system", true)
			require.NoError(t, err)
			assert.Empty(t, rep.StuckFinalizers)
			assert.Empty(t, rep.CloudResourcesDeleted)
			assert.Empty(t, rep.FinalizersCleared)
			assert.Empty(t, rep.Blocked)
			assert.Empty(t, rep.ManualCommands)
		})
	}
}
