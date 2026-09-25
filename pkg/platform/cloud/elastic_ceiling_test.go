package cloud_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
)

// The managed clouds must report an UNKNOWN ceiling without touching the API:
// their autoscalers can add a node bigger than any present today, so today's
// largest node does not bound what can schedule. Passing a nil Clients proves
// no API access — any call would panic.
func TestManagedCloudsReportUnknownCeiling(t *testing.T) {
	for _, s := range []cloud.Strategy{gke.Strategy{}, eks.Strategy{}, aks.Strategy{}} {
		t.Run(s.Key(), func(t *testing.T) {
			c, h, err := s.SchedulingCeiling(context.Background(), cloud.Clients{})
			require.NoError(t, err)
			assert.False(t, c.Known, "an autoscaling cloud has no provable ceiling")
			assert.NotEmpty(t, c.Source, "Source must explain why the check is skipped")
			assert.False(t, h.Known)
		})
	}
}
