package eks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestEKSEnsureGatewayControllerDelegatesOverride(t *testing.T) {
	res, err := Strategy{}.EnsureGatewayController(context.Background(),
		cloud.GatewayControllerParams{GatewayClassOverride: "custom"})
	require.NoError(t, err)
	assert.True(t, res.Proceed)
	assert.Equal(t, "custom", res.GatewayClass, "delegation must return the override verbatim")
}
