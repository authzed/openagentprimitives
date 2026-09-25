package local

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// SchedulingCeiling reads the cluster's own nodes: the desktop k3s VM does not
// grow, so the largest node present is the real ceiling. See
// cloud.NodeSchedulingCeiling for why that is unsafe on an autoscaling cloud.
func (Strategy) SchedulingCeiling(ctx context.Context, cl cloud.Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	return cloud.NodeSchedulingCeiling(ctx, cl)
}
