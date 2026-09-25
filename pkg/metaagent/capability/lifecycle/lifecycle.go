// Package lifecycle is the metaagent's subtractive capability: stop the run,
// revoke a phase approval.
//
// Registered by blank import, per the repo's standard registry shape.
package lifecycle

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
)

func init() { capability.Register(&Capability{}) }

// Capability effects lifecycle changes: cancelling the session, and revoking a
// phase's approval.
//
// SUBTRACTIVE ONLY, and that is the whole design. Every action here takes
// authority away, so every one is Narrowing and applies with no approval —
// which is the fail-safe direction: a user saying "stop" during an incident
// must not be made to wait on a second click.
//
// Subtractive does NOT mean harmless, which is why the gate is `approve` rather
// than `interact`. Without approver standing, a hostile thread participant could
// cancel everyone's work.
type Capability struct{}

func (*Capability) Name() string    { return "lifecycle" }
func (*Capability) DefaultOn() bool { return true }

// Permission gates on approver standing. See the type comment: subtractive is
// fail-safe, not harmless.
func (*Capability) Permission() string { return "approve" }

const (
	actionCancelSession  = "cancel_session"
	actionUnapprovePhase = "unapprove_phase"
)

// Actions is the closed vocabulary. Every entry is Narrowing, and
// TestLifecycle_everyActionIsNarrowing asserts that rather than trusting it —
// a Widening action mislabelled here would bypass approval entirely, which is
// why Direction is the security-critical field in any capability.
func (*Capability) Actions() []metaagent.Action {
	return []metaagent.Action{
		{Capability: "lifecycle", Name: actionCancelSession, Direction: metaagent.Narrowing},
		{Capability: "lifecycle", Name: actionUnapprovePhase, Direction: metaagent.Narrowing},
	}
}

// Ceiling is permitted and unbounded: there is no administrative limit on how
// much a session may be stopped. Permitted stays true or the capability would
// be inert.
func (*Capability) Ceiling(metaagent.StateSnapshot) capability.Bound {
	return capability.Bound{Permitted: true}
}

// Apply effects the decision.
//
// An unwired effector is REPORTED, never treated as success. Losing an effect
// the user asked for and saying nothing is precisely the failure the
// no-silent-errors rule exists to prevent — and here the user believes the
// session stopped when it did not.
func (c *Capability) Apply(ctx context.Context, env capability.Env, d metaagent.Decision) error {
	switch d.Action {
	case actionCancelSession:
		if env.CancelSession == nil {
			return fmt.Errorf("metaagent/lifecycle: cancel_session is not wired for %s; "+
				"the session was NOT cancelled", env.Session)
		}
		if err := env.CancelSession(ctx, "cancelled via the metaagent"); err != nil {
			return fmt.Errorf("metaagent/lifecycle: cancel %s: %w", env.Session, err)
		}
		notify(ctx, env, "Session cancelled.")
		return nil

	case actionUnapprovePhase:
		// Zero is a legitimate phase index, so the range check is explicit
		// rather than a zero test. A negative index names no phase; refuse it
		// here rather than hand it to an effector that would either error
		// obscurely or, worse, act on it.
		if d.TargetIndex < 0 {
			return fmt.Errorf("metaagent/lifecycle: phase index %d names no phase", d.TargetIndex)
		}
		if env.UnapprovePhase == nil {
			return fmt.Errorf("metaagent/lifecycle: unapprove_phase is not wired for %s; "+
				"phase %d is STILL APPROVED", env.Session, d.TargetIndex)
		}
		if err := env.UnapprovePhase(ctx, d.TargetIndex); err != nil {
			return fmt.Errorf("metaagent/lifecycle: unapprove phase %d on %s: %w",
				d.TargetIndex, env.Session, err)
		}
		notify(ctx, env, fmt.Sprintf("Phase %d is no longer approved.", d.TargetIndex))
		return nil

	default:
		// Reachable only through a bug — DirectionOf already refuses an
		// unregistered action — so it must not fall through to a default
		// effect.
		return fmt.Errorf("metaagent/lifecycle: no action %q; refusing to guess at an effect", d.Action)
	}
}

// notify surfaces an auto-applied narrowing. Best-effort: a missing notifier
// must not undo an effect that already happened, but every auto-applied change
// that CAN be announced is, because a session that quietly shrank is
// indistinguishable from a broken one.
func notify(ctx context.Context, env capability.Env, body string) {
	if env.Notify != nil {
		env.Notify(ctx, body)
	}
}
