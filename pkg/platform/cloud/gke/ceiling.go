package gke

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/schedfit"
)

// SchedulingCeiling reports an unknown ceiling. GKE node pools autoscale, and
// Autopilot's node auto-provisioning will create a machine type larger than
// anything currently in the cluster, so today's largest node does not bound
// what can eventually schedule. Skipping here is deliberate: a false
// "unschedulable" would abort a valid install, while a missed one still lands
// on the operator's runtime fast-fail.
func (Strategy) SchedulingCeiling(context.Context, cloud.Clients) (schedfit.Ceiling, schedfit.Headroom, error) {
	return schedfit.Ceiling{
		Source: "GKE node pools autoscale; a request above today's largest node may still schedule",
	}, schedfit.Headroom{}, nil
}
