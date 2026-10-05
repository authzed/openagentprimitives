package goals

import (
	"context"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// RunCost retains the bounded root's cumulative estimate, never a hard USD
// budget or proof of payment. The root total already includes ByModel and
// ByTool; summing those buckets again would count the same spend twice.
// Final requires a terminal runner observation. Missing estimates and forced
// termination remain visibly incomplete, even after session garbage collection.
type RunCost struct {
	SessionUID string                   `json:"sessionUID"`
	Estimate   *v1.EstimatedSessionCost `json:"estimate,omitempty"`
	Final      bool                     `json:"final"`
	Reason     string                   `json:"reason,omitempty"`
	ObservedAt time.Time                `json:"observedAt"`
}

// CostStore accepts operator observations under the current dispatch fence.
// Agents cannot submit or replace accounting through a goal result proposal.
type CostStore interface {
	RecordCost(context.Context, Occurrence, RunCost, time.Time) (Occurrence, error)
}
