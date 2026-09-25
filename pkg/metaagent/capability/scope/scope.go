// Package scope is the metaagent's session-scope capability.
//
// Registered by blank import, per the repo's standard registry shape.
package scope

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/metaagent"
	"github.com/authzed/openagentprimitives/pkg/metaagent/capability"
)

func init() { capability.Register(&Capability{}) }

// Capability narrows or widens the session scope.
//
// It DELEGATES the actual delta to the existing extract → classify → apply
// chain rather than reimplementing it — "the existing behavior, unchanged
// except for its new trigger". What this module contributes is the ambient
// trigger, the standing gate and the direction rule; the resource refs never
// travel on a Decision, so ids derived from untrusted turn text stay on the
// path that already validates them (ClassifySkipped checks the type against the
// closed envelope and requires the REQUESTER to already hold the permission on
// that id).
type Capability struct{}

func (*Capability) Name() string       { return "scope" }
func (*Capability) DefaultOn() bool    { return true }
func (*Capability) Permission() string { return "manage_scope" }

const (
	actionNarrow = "narrow"
	actionWiden  = "widen"
)

// Actions declares the two directions honestly. `widen` hands the session reach
// it did not have, so it can never auto-apply; mislabelling it Narrowing would
// bypass approval entirely, which is why Direction is the field review must
// check in any capability.
func (*Capability) Actions() []metaagent.Action {
	return []metaagent.Action{
		{Capability: "scope", Name: actionNarrow, Direction: metaagent.Narrowing},
		{Capability: "scope", Name: actionWiden, Direction: metaagent.Widening},
	}
}

// Ceiling is the AgentClass envelope, enforced downstream by ClassifySkipped:
// a ref whose type is not in the envelope is dropped there with a reason. There
// is no numeric or enumerated bound to express here, so this reports permitted
// and leaves the real bounding to the machinery that owns it.
func (*Capability) Ceiling(metaagent.StateSnapshot) capability.Bound {
	return capability.Bound{Permitted: true}
}

// Apply runs the scope chain and, for an approved widening, carries it to the
// second bound.
func (c *Capability) Apply(ctx context.Context, env capability.Env, d metaagent.Decision) error {
	switch d.Action {
	case actionNarrow, actionWiden:
	default:
		return fmt.Errorf("metaagent/scope: no action %q; refusing to guess at an effect", d.Action)
	}

	if env.ApplyScope == nil {
		return fmt.Errorf("metaagent/scope: the scope chain is not wired for %s; "+
			"scope was NOT changed", env.Session)
	}
	added, err := env.ApplyScope(ctx)
	if err != nil {
		return fmt.Errorf("metaagent/scope: apply scope change on %s: %w", env.Session, err)
	}

	if d.Action == actionNarrow {
		// A narrowing has no second bound to carry. Rewriting the plan to
		// REMOVE reach would be actively wrong: the phase ceiling is already a
		// narrowing gate, and stripping a handle the agent may still hold
		// legitimately in a later phase would break work nobody asked to stop.
		notify(ctx, env, "Session scope narrowed.")
		return nil
	}

	if len(added) == 0 {
		// The chain applied nothing — every ref was skipped as out of envelope,
		// or the requester held none of them. The two bounds move together or
		// not at all: widening a phase for reach the session never got would
		// leave a ceiling entry nothing backs.
		notify(ctx, env, "Nothing was added to the session scope.")
		return nil
	}

	if env.WidenActivePhase == nil {
		// No plan gate in this session, so there IS no second bound. The phase
		// half is an addition to the scope half, never a precondition for it.
		notify(ctx, env, "Session scope widened.")
		return nil
	}
	if err := env.WidenActivePhase(ctx, added); err != nil {
		// The session is now SPLIT — scope widened, phase not — and the user
		// must be told, or they will believe one approval covered both right up
		// until the agent is denied.
		return fmt.Errorf("metaagent/scope: scope was widened on %s but the active phase "+
			"could not be: %w", env.Session, err)
	}
	notify(ctx, env, "Session scope widened, and the active phase now admits it.")
	return nil
}

func notify(ctx context.Context, env capability.Env, body string) {
	if env.Notify != nil {
		env.Notify(ctx, body)
	}
}
