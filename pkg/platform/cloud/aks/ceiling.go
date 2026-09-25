package aks

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// SchedulingCeiling reports an unknown ceiling. AKS node pools autoscale and
// can add a larger VM size than anything in the cluster today. See the EKS
// twin for why a fixed pool is not special-cased.
func (Strategy) SchedulingCeiling(context.Context, cloud.Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	return schedfit.Ceiling{
		Source: "AKS node pools autoscale; a request above today's largest node may still schedule",
	}, schedfit.Headroom{}, nil
}
