package runner

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// UIToolOptions builds the uicomponents.Options a Tier-1 fragment is
// validated against, from the SAME materialized Loop.AppTools the per-call
// gate in handleAppToolCallReq consults.
//
// Both maps are derived here rather than passed in, so the validator and the
// runtime gate cannot disagree about which tools exist:
//
//   - GrantedTools: every key of appTools. Those keys are already
//     synthesize.NormalizeName output (see MaterializeAppTools), which is why
//     NormalizeToolName is set to the same function — a Binding.Ref carries
//     no CRD pattern of its own and reaches here as plain author/agent text.
//   - ReadonlyTools: the subset couldEverBeReadonly admits — a CEILING on the
//     per-call predicate handleAppToolCallReq applies, not the predicate
//     itself (that gate resolves against the REAL call args; this function
//     validates a DOCUMENT, which has none to resolve against). A data
//     binding the validator accepts is one the gate MAY run for some
//     arguments — not one it will always run.
//
// A nil or empty map yields zero grants, which is the fail-closed default
// uicomponents.Options already documents for both fields.
func UIToolOptions(appTools map[string]tool.Tool) uicomponents.Options {
	o := uicomponents.DefaultOptions()
	o.NormalizeToolName = synthesize.NormalizeName
	if len(appTools) == 0 {
		return o
	}
	o.GrantedTools = make(map[string]bool, len(appTools))
	o.ReadonlyTools = make(map[string]bool, len(appTools))
	for name, t := range appTools {
		o.GrantedTools[name] = true
		if couldEverBeReadonly(t) {
			o.ReadonlyTools[name] = true
		}
	}
	return o
}

// couldEverBeReadonly reports whether ANY argument map could make this tool
// satisfy the per-call auto-run predicate: the origin's own readOnlyHint
// (argument-independent, so it is decidable statically) AND a readonly
// StateImpact reachable either from the static fallback or from some
// PermissionVariant.
//
// It is a CEILING, not the predicate. handleAppToolCallReq resolves the
// variant against the REAL arguments on every call; this function has no
// arguments to resolve against, because it validates a DOCUMENT rather than
// a call. So the containment is one-way: every tool the per-call gate can
// ever auto-run is in this set, and a tool in this set may still be denied
// for the specific arguments a viewer's page sends.
func couldEverBeReadonly(t tool.Tool) bool {
	if !serverReadOnlyHint(t) {
		return false
	}
	if t.Permission().StateImpact == authz.Readonly {
		return true
	}
	for _, v := range t.PermissionVariants() {
		if v.Check.StateImpact == authz.Readonly {
			return true
		}
	}
	return false
}

// AttachUIView wires a uiview.Runtime to a live Loop. It is the ONE place
// the late-bound collaborators are set, so internal/cmd/runner and test/e2e's
// in-process factory cannot drift — the same reason MaterializeAppTools is a
// shared function rather than duplicated startup code.
//
// Ordering, which is not optional: capability.Assemble runs BEFORE
// Loop.AppTools is materialized, so the Runtime must be constructed first
// (with nil collaborators) and attached here once the Loop exists. Nothing
// can call into it in between — a meta tool's Execute only runs inside the
// loop.
func AttachUIView(rt *uiview.Runtime, l *Loop) {
	rt.ToolOptions = func() uicomponents.Options {
		return UIToolOptions(l.AppTools)
	}
	// rt.Publish closes over rt's OWN Namespace/Session (never l's — a Loop
	// carries no session identity of its own) so uiview.Runtime.Write's
	// ui_view_update push always names the session the fragment was actually
	// written for, and reads l.UIPublish indirectly through l so a kubectl or
	// test Loop that never sets it (nil) degrades this to memory-only, the
	// same fail-soft posture uiActionRecorder's identical wiring documents.
	rt.Publish = func(ctx context.Context, env channelevents.Envelope) error {
		if l.UIPublish == nil {
			return nil
		}
		return l.UIPublish(ctx, rt.Namespace, rt.Session, env)
	}
}
