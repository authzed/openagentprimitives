package authz

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Checker is the seam pkg/authz uses to invoke a Check from within
// orchestration helpers such as BindClassDefaults. Production callers pass a
// CheckerFunc wrapping the backend checker (toolcheck.Checker.CheckToolCall
// for SpiceDB); tests inject stubs.
type Checker interface {
	// Check decides whether the subject carried in `in` holds perm's
	// PermissionCheck on the resource that check resolves to, and returns the
	// verdict as a Result — never an error.
	//
	// Every failure mode collapses into OutcomeDenied with an explanatory
	// Message: a SpiceDB RPC error, an unwired client, an unresolvable
	// resource-ID template, and a genuine "no" are indistinguishable to the
	// caller BY DESIGN, so the only reachable behaviour is fail-CLOSED. A
	// caller must not read a denial as "SpiceDB is fine and the answer is no".
	Check(ctx context.Context, perm Permission, in Inputs) Result
}

// CheckerFunc adapts a plain function to the Checker interface.
type CheckerFunc func(ctx context.Context, perm Permission, in Inputs) Result

// Check implements Checker.
func (f CheckerFunc) Check(ctx context.Context, p Permission, in Inputs) Result {
	return f(ctx, p, in)
}

// BoundEntitySpec carries the fields BindClassDefaults and FillToolArgs need
// from spiceboxv1alpha1.BoundEntityType. It is duplicated rather than imported
// because pkg/authz cannot import pkg/apis/v1alpha1 — v1alpha1 imports pkg/authz
// for Permission / PermissionVariant.
type BoundEntitySpec struct {
	ResourceType string
	Permission   string
	Defaults     []string
	AutoFillArgs []AutoFillArgSpec
	// FillFrom narrows the ways an instance may come to occupy this slot.
	// Empty narrows nothing — see AllowsFill.
	FillFrom []string
	// ValueTransforms is the chain a free-form value passes through to become
	// the object id, from AgentClass.status.resolvedSlots[].valueTransforms.
	// Empty means this slot is not value-keyed — its ids are already distinct
	// resources and are used as-is.
	ValueTransforms []string
	// Requires are the compiled predicates over the facts a candidate arrived
	// with — each with the two messages its author wrote — from AgentClass slot
	// `requires[]`. EVERY one must evaluate to precondition.Satisfied before an
	// instance may occupy this slot; a Refused or Undetermined verdict holds it
	// out (see checkPreconditions).
	//
	// Empty is the overwhelmingly common case and means exactly what it did
	// before preconditions existed: nothing extra gates this slot, and no fact
	// read is performed for its candidates.
	//
	// Carried as compiled programs rather than as expression strings because a
	// predicate that will not compile must stop an AgentClass at admission
	// (pkg/controllers/agentclass/slot_declaration.go), never at dispatch, where
	// it would present as a slot that mysteriously never binds.
	//
	// The messages ride ALONGSIDE the program rather than in a parallel slice
	// because dispatch quotes them back to the agent to explain why the slot is
	// empty (ExplainPreconditionDenial), and a message matched to the wrong
	// predicate is worse than none: it names a fix for a rule that is not the
	// one holding the slot shut.
	//
	// A builder that omits this field yields slots whose gate NEVER FIRES, in
	// silence — which is why slotspec.FromSlots is the only thing that
	// constructs a BoundEntitySpec, and why a guard test refuses a second
	// construction site.
	Requires []precondition.Rule
}

// AutoFillArgSpec mirrors v1alpha1.EntityAutoFillArg for FillToolArgs.
type AutoFillArgSpec struct {
	ArgName         string
	ToolNamePattern string
}

// BindClassDefaults Checks each BoundEntitySpec.Defaults against SpiceDB and
// binds the allowed ones into the session, in two places:
//
//  1. A SLOT GRANT per instance — the authorization. This is what the tool's
//     own PermissionCheck consults at dispatch, and what FillToolArgs reads.
//  2. A ScopeResource{Source: SourceDefault} in the SessionScope memory doc —
//     the Layer-2 narrowing, a SEPARATE enforcement layer.
//
// Writing both is deliberate, not the two-sources-of-truth the design rejects.
// The rejected shape was autofill reading a different store from the one that
// authorizes; that is gone — autofill now reads the grants. What remains is one
// fact feeding two INDEPENDENT gates, and the two can only ever intersect: a
// grant without a ScopeResource is denied by scope, a ScopeResource without a
// grant is denied by SpiceDB. Drift fails closed in both directions.
//
// An earlier revision of this comment called the scope write transitional and
// promised it would come out once routeViaSessionGrant was retired. It does not
// come out, and the reason is worth recording so nobody re-attempts it:
// CheckScopeWithRefs narrows PER TYPE. Once any binding path puts a type into
// scope.Resources, every ref of that type must match an ID or pattern or it is
// denied. Since all four fill sources share bindSlots, dropping the scope write
// from one of them would deny ITS instances whenever another source had written
// a resource of the same type — the defaults would break as soon as a query or
// ask fill touched the same type. Dropping it from all of them is possible, but
// that is deleting Layer 2's instance narrowing, not finishing a migration.
//
// The scope write goes FIRST and is not conditional on the grant write, which
// inverts the usual "authorize first" instinct for a concrete reason. A slot
// grant needs the resource's definition to carry the slot_grant relation, and
// the schema composer skips any type the schema does not declare — so the grant
// write fails against exactly those types. Aborting there would drop the
// ScopeResource too, and a type ABSENT from scope.Resources means "scope does
// not narrow this type" — so the abort would WIDEN Layer-2, not fail closed.
// Writing scope first and returning the grant error keeps the narrowing intact
// and puts the gap in front of an operator.
//
// Denied defaults are dropped with an INFO log.
//
// The write is idempotent: grants are TOUCHed, and each call reads the current
// SessionScope, appends allowed defaults (using scope.ApplyDelta's appendUnique
// logic), and Puts the updated doc. Re-running after a restart or reconcile is
// safe.
//
// Parameters: nil chk / empty subject / nil mem / empty entities is a no-op.
func BindClassDefaults(
	ctx context.Context,
	mem memory.Memory,
	memScope memory.Scope,
	sess SessionRef,
	entities []BoundEntitySpec,
	chk Checker,
	w RelWriter,
	subject string,
	now func() time.Time,
	sessionExpiration time.Duration,
) error {
	if chk == nil || subject == "" || mem == nil {
		return nil
	}
	if len(entities) == 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	logger := log.FromContext(ctx).WithName("authz.bind_defaults")

	var cands []slotCandidate
	for _, et := range entities {
		if len(et.Defaults) > 0 && !et.AllowsFill(FillDefault) {
			// The class pinned IDs and then declared a fillFrom that excludes
			// the source those IDs bind through. The AgentClass reconciler
			// rejects that combination, so reaching it means the object predates
			// the rule or was written past validation — bind nothing and say so,
			// rather than honour a declaration the class contradicts.
			logger.Info("slot has defaults but fillFrom excludes default; binding none of them",
				"resourceType", et.ResourceType, "defaults", len(et.Defaults), "fillFrom", et.FillFrom)
			continue
		}
		for _, def := range et.Defaults {
			id, err := NewObjectID(def, et.ValueTransforms)
			if err != nil {
				// A pinned default that cannot become an object id binds nothing —
				// never silent, since a class author needs to know why a default
				// they configured is not taking effect.
				logger.Info("class default could not be derived into an object id; skipped",
					"resourceType", et.ResourceType, "err", err.Error())
				continue
			}
			cands = append(cands, slotCandidate{
				ResourceType: et.ResourceType,
				Permission:   et.Permission,
				ResourceID:   id,
				RawID:        def,
				Requires:     et.Requires,
			})
		}
	}
	bound := admissibleCandidates(ctx, mem, memScope, chk, subject, string(scope.SourceDefault), logger, cands)
	return bindSlots(ctx, mem, memScope, sess, w, scope.SourceDefault, logger, now, SlotGrantExpiry(now(), sessionExpiration), bound)
}
