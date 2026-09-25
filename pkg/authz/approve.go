package authz

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ApproverResourceRef identifies a source resource whose #owner must include
// the approver (the per-gate "source permission"). Uses Type rather than Kind
// to match SpiceDB resource-type nomenclature.
type ApproverResourceRef struct {
	Type string
	ID   string
	// Permission is the resource type's declared approverPermission — the
	// permission that confers standing to approve requests against THIS type.
	// Empty means "owner", which is what every ref meant before this field
	// existed, so durable records written earlier keep their meaning.
	//
	// It has to travel with the ref. The runner expands exactly this
	// permission when it raises the card, and the wire type carries it for the
	// same reason; a click-time gate that rebuilt the ref without it checked a
	// literal "owner" and so disagreed with raise-time about who may approve.
	// On a type declaring maintainer or admin that refuses the person the
	// prompt was routed to and admits an owner nobody asked.
	Permission string
}

// ApproverChecker is the SpiceDB-side dependency for click-time approver authz.
type ApproverChecker interface {
	// CheckApprove reports whether canonicalID holds agentsession#approve on
	// ns/name — the whole gate when an approval names no source resource.
	// fullyConsistent=true reads at head. Returns (false, err) on failure and
	// CheckApproverAuthorized propagates it as a refusal (fail-CLOSED).
	CheckApprove(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)

	// CheckOwnerOnResource reports whether canonicalID holds #owner on
	// <resType>:<resID>. Retained because callers outside this gate use it;
	// CheckApproverAuthorized itself goes through CheckOnResource so a type's
	// declared approverPermission is honoured.
	CheckOwnerOnResource(ctx context.Context, resType, resID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)

	// CheckOnResource reports whether canonicalID holds <permission> on
	// <resType>:<resID> — the gate when an approval names source resources,
	// one call per resource, each with the permission that resource's type
	// declared. Returns (false, err) on failure; the caller keeps polling the
	// remaining resources and surfaces the first error only if NO resource
	// answered yes, so an outage can never manufacture an approval.
	CheckOnResource(ctx context.Context, resType, resID, permission string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
}

// CheckApproverAuthorized is the click-time gate.
//
//   - resources present ⇒ the clicker must be #owner on AT LEAST ONE source
//     resource (any owner may approve; quorum = 1). agentsession#approve is
//     NOT consulted.
//   - resources empty ⇒ owner-only gate: agentsession#approve alone decides.
//
// Fail-closed: a check error with no affirmative ownership ⇒ not authorized
// (the error is returned).
//
// RATIONALE — do NOT "harden" the resource branch into `approve AND owner`.
// A resource-scoped approval solicits the RESOURCE OWNER's consent, and that
// owner usually has no relationship to the session at all (the owner resolver
// writes only the requester as session owner), so demanding session standing on
// top of ownership rejects the one person the prompt was routed to. This has
// shipped as a production outage once, surfacing as "no one has standing to
// approve". Session standing is not sufficient either: a session owner must
// never self-approve access to someone else's resource.
//
// Across multiple resources, ownership of ANY listed resource suffices —
// approval is delivered to all owners and any one of them vouches. Raise-time
// eligibility mirrors this rule in ResolveApprovers; change both or neither.
func CheckApproverAuthorized(ctx context.Context, c ApproverChecker, ns, name string, resources []ApproverResourceRef, canonicalID identity.CanonicalUserID) (bool, error) {
	// Fail closed on an unwired checker, as every sibling check in this package
	// does. Without this an operator built without an ApproverChecker panics on
	// the first approval instead of denying it.
	if c == nil {
		return false, nil
	}
	if len(resources) == 0 {
		return c.CheckApprove(ctx, ns, name, canonicalID, true)
	}
	var firstErr error
	for _, r := range resources {
		// Each resource is asked with ITS OWN declared permission. Quorum is
		// still one — any single qualifying resource vouches — and session
		// standing is still not consulted on this branch. Only which
		// permission is named changes.
		perm := r.Permission
		if perm == "" {
			perm = "owner"
		}
		ok, err := c.CheckOnResource(ctx, r.Type, r.ID, perm, canonicalID, true)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, firstErr
}
