package goals

import (
	"context"
	"fmt"
	"reflect"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func (d *Dispatcher) captureCost(ctx context.Context, o domain.Occurrence, sess *v1.AgentSession, exists, terminal bool) (domain.Occurrence, error) {
	// Every snapshot is from the exact root attached to this occurrence. Current
	// bounded runs expose no delegation tool, so this root is the complete tree.
	if exists && (!reflect.DeepEqual(sess.Spec.GoalExecution, ref(o)) || string(sess.UID) != o.SessionUID || sess.Namespace != o.Domain.Namespace || sess.Name != o.SessionName) {
		return o, fmt.Errorf("refusing accounting from unrelated goal session")
	}
	if o.SessionUID == "" || (o.Cost != nil && o.Cost.Final) {
		return o, nil
	}
	c := domain.RunCost{SessionUID: o.SessionUID, Reason: "session_missing"}
	if exists {
		c.Estimate = sess.Status.EstimatedCost.DeepCopy()
		c.Reason = "estimate_unavailable"
		if c.Estimate != nil {
			if terminal {
				reason, err := d.runnerOutcome(ctx, sess)
				if err != nil {
					return o, err
				}
				terminal = reason == domain.RunSessionEnded
			}
			c.Final = terminal
			c.Reason = "runner_active_or_interrupted"
			if terminal {
				c.Reason = ""
			}
		}
	}
	// A missing object/estimate must retain a previously captured partial value.
	if c.Estimate == nil && o.Cost != nil {
		return o, nil
	}
	if o.Cost != nil && c.Estimate != nil && o.Cost.Estimate != nil && c.Estimate.AsOf.Before(&o.Cost.Estimate.AsOf) {
		return o, nil
	}
	// A runner restart can reset in-memory counters under the same root UID.
	// Preserve the observed lower bound and keep incompleteness sticky: later
	// counters catching up cannot prove the missing earlier spend was included.
	if o.Cost != nil && o.Cost.Estimate != nil && c.Estimate != nil {
		regressed := c.Estimate.Currency != o.Cost.Estimate.Currency || c.Estimate.AmountMicroUSD < o.Cost.Estimate.AmountMicroUSD
		if regressed {
			c.Estimate = o.Cost.Estimate.DeepCopy()
		}
		if regressed || o.Cost.Reason == "accounting_regressed" {
			c.Final = false
			c.Reason = "accounting_regressed"
		}
	}
	store, ok := d.Store.(domain.CostStore)
	if !ok {
		return o, fmt.Errorf("goal dispatch requires durable cost observations")
	}
	observed, err := store.RecordCost(ctx, o, c, d.now())
	if err != nil {
		return o, err
	}
	return observed, nil
}
