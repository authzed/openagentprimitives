// Package reconcile provides a shared phase-based reconciler skeleton
// used by every controller in pkg/controllers/*. Each reconciler
// builds a list of Phases, hands them to RunPhases, and the skeleton
// handles status persistence + early-stop semantics so reconcilers
// stay focused on their phase logic.
package reconcile

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Phase is one step in a reconciler. Phases mutate the in-memory CR
// (typically by setting conditions via pkg/controllers/conditions).
// The returned Outcome controls flow:
//   - Continue (default zero value): keep going to the next Phase.
//   - Stop: skip remaining Phases; the skeleton still persists status.
//   - Err: returned as the reconcile error after status persistence.
type Phase func(ctx context.Context) Outcome

// Outcome controls Phase chain flow.
type Outcome struct {
	Stop bool  // skip remaining phases (status is still persisted)
	Err  error // returned to controller-runtime; status still persisted
}

// Continue is the zero-value outcome; sugar for explicit returns.
func Continue() Outcome { return Outcome{} }

// StopAfter halts the chain after the current phase. Use when the
// phase already set a terminal condition; the skeleton will persist.
func StopAfter() Outcome { return Outcome{Stop: true} }

// FailWith halts the chain and bubbles err up to controller-runtime
// after status persistence.
func FailWith(err error) Outcome { return Outcome{Stop: true, Err: err} }

// RunPhases runs phases in order. Always calls Status().Update on
// `obj` at the end (even on Stop or Err). The Status update error is
// returned only if no phase already produced an error (an existing
// Err takes precedence so transient API errors don't mask phase
// errors).
func RunPhases(
	ctx context.Context,
	cli client.Client,
	obj client.Object,
	phases []Phase,
) error {
	var phaseErr error
	for _, p := range phases {
		out := p(ctx)
		if out.Err != nil {
			phaseErr = out.Err
		}
		if out.Stop || out.Err != nil {
			break
		}
	}
	if updErr := cli.Status().Update(ctx, obj); updErr != nil && phaseErr == nil {
		return updErr
	}
	return phaseErr
}
