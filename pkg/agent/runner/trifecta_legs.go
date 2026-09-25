package runner

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/trifecta"
)

// trifectaStandingLegs derives legs A and B for THIS session — the standing
// properties of the data bound into it.
//
// The hook decides leg C from the call in front of it, so what it needs from
// here is only "what is this session holding, and is any of it dangerous to
// hold". trifecta.Derive computes leg C too, from the surface passed below;
// the hook ignores that value by contract, and passing the real surface is
// still right because Derive short-circuits on it before touching SpiceDB — a
// session that cannot act needs no lookups at all.
//
// EVERY unresolvable lookup is an error, never a false leg. A leg that
// silently read false would be the trifecta failing to fire with no denial, no
// hold and nothing anywhere to notice — the worst outcome this package can
// produce. The hook's own mode switch decides what an error means.
func (l *Loop) trifectaStandingLegs(ctx context.Context) (trifecta.Legs, error) {
	if l.TrifectaBoundTags == nil {
		return trifecta.Legs{}, fmt.Errorf("trifecta: bound-tag lookup is not wired")
	}
	tags, err := l.TrifectaBoundTags(ctx)
	if err != nil {
		return trifecta.Legs{}, fmt.Errorf("trifecta: bound tags: %w", err)
	}
	return trifecta.Derive(ctx, l.TrifectaDeps, trifecta.Handoff{
		Child:        authz.SessionRef{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name},
		BoundInputs:  tags,
		ChildSurface: l.PlanGateSurface,
	})
}

// closureDenied reports whether some member of this session's delegation
// closure has been denied — the §2.8 structural precondition the trifecta gate
// refuses on in every mode.
//
// Read off the session's OWN status, live. The runner cannot compute the
// closure (its Role grants no list on agentsessions), so the operator stamps
// the fact onto every member and this reads the one object the runner may Get.
//
// An unreadable session is an ERROR, not a clean answer. The gate refuses in
// every mode on this flag, so reporting false on a transient API failure would
// switch off a control precisely when the cluster is already unhealthy.
//
// Nil status means NOT YET EVALUATED, which is reported as not-denied. That is
// the one place this answers optimistically, and deliberately: the flag is
// set-only and stamped when a denial happens, so nil is the state of every
// session in a closure where nothing has ever been refused.
func (l *Loop) closureDenied(ctx context.Context) (bool, error) {
	if l.SessionStatusReader == nil {
		return false, fmt.Errorf("trifecta: closure-denial reader is not wired")
	}
	denied, err := l.SessionStatusReader(ctx)
	if err != nil {
		return false, fmt.Errorf("trifecta: reading this session's closure-denial flag: %w", err)
	}
	return denied, nil
}

// trifectaCallImpact reports the StateImpact of the call in front of the hook.
//
// Leg C is readwrite or external; readonly is leg B's territory, so a
// read-only call can never complete the trifecta however sensitive its data.
//
// Resolved from the TOOL rather than from the session's surface, because the
// surface's StateImpact is the MAX across every producer of a handle — right
// for asking "could this session ever act", wrong for "is this particular call
// an action". Using the surface here would make every call by a session that
// holds one write tool look consequential.
//
// An unresolvable tool is an ERROR, not a readonly default. The hook fails
// closed in enforcing mode, and defaulting to readonly here would hand it a
// clean leg C for a call nobody could classify.
func (l *Loop) trifectaCallImpact(toolName string, args map[string]any) (authz.StateImpact, error) {
	t, ok := l.lookupTool(toolName)
	if !ok {
		return "", fmt.Errorf("trifecta: tool %q is not in this session's catalog", toolName)
	}
	// permissionForTool, NOT ResolvePermissionForArgs: the former asks
	// tool.PerCallPermission first, and for a sandbox tool that is the only
	// resolver that parses argv the way dispatch does. The CEL-variant path
	// disagrees with it in the under-reporting direction, by two mechanisms
	// the sandbox package states outright — every argument-level variant
	// records the SUBCOMMAND predicate rather than its own, so an argv that
	// mutates matches the same predicate as one that reads and takes whichever
	// variant was emitted first; and the predicate cannot skip a global flag's
	// VALUE, so an argv led by one matches no variant at all. Both fall
	// through to the toolkit default, which is the very bug PermissionVariants
	// exists to fix. A non-matching predicate is not an error, so this failed
	// silently OPEN: leg C read Passthrough for an outbound write and the
	// trifecta never fired in any mode.
	perm, err := l.permissionForTool(t, args)
	if err != nil {
		return "", fmt.Errorf("trifecta: resolving the permission for %q: %w", toolName, err)
	}
	return perm.StateImpact, nil
}
