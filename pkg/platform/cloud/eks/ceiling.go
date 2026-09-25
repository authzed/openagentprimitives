package eks

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// SchedulingCeiling reports an unknown ceiling. An EKS managed node group (or
// Karpenter) can add a larger instance type than anything in the cluster today.
// A fixed node group technically has a hard ceiling, but telling the two apart
// needs AWS API calls, and guessing wrong aborts a valid install — so this
// stays unknown and the operator's runtime fast-fail carries that case.
func (Strategy) SchedulingCeiling(context.Context, cloud.Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	return schedfit.Ceiling{
		Source: "EKS node groups autoscale; a request above today's largest node may still schedule",
	}, schedfit.Headroom{}, nil
}
