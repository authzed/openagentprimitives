package authz

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Lookuper is the SpiceDB-side dependency Lookup* wraps. Implemented by
// *spicedb.Client; abstracted for tests.
type Lookuper interface {
	// LookupSubjects expands a subject-set expression (e.g. "repo:r1#owner") to
	// the canonical subjects currently in it. An empty result means nobody
	// holds it; an error means unknown, and callers must not read the two the
	// same way — ResolveApprovers propagates the error rather than reporting
	// "no one has standing".
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)

	// LookupInteractSubjects expands the session's interact set to its current
	// members — for addressing a broadcast, not for deciding one user's access.
	LookupInteractSubjects(ctx context.Context, ns, name string) ([]string, error)

	// LookupSubjectIncludes reports whether canonicalID is currently a member of
	// the subject-set expression. Returns (false, err) on failure; callers treat
	// that as not-a-member (fail-CLOSED).
	LookupSubjectIncludes(ctx context.Context, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error)
}

// LookupApprovers returns every canonical subject currently included in
// the given subject-set expression. Used by approval orchestration to
// resolve "who can approve" for a denial.
func LookupApprovers(ctx context.Context, l Lookuper, subjectSet string) ([]string, error) {
	if l == nil {
		return nil, nil
	}
	return l.LookupSubjects(ctx, subjectSet)
}

// LookupInteractParticipants returns every canonical subject currently
// authorized to interact with the session.
func LookupInteractParticipants(ctx context.Context, l Lookuper, scope SessionRef) ([]string, error) {
	if l == nil {
		return nil, nil
	}
	return l.LookupInteractSubjects(ctx, scope.Namespace, scope.Name)
}

// LookupSubjectIncludes returns true if canonicalID is currently a
// member of the given subject-set expression.
func LookupSubjectIncludes(ctx context.Context, l Lookuper, subjectRef string, canonicalID identity.CanonicalUserID) (bool, error) {
	if l == nil {
		return false, nil
	}
	return l.LookupSubjectIncludes(ctx, subjectRef, canonicalID)
}

// ResolveApprovers returns the eligible approver subjects for an approval,
// and empty=true when no one is eligible (fail-closed "no one has standing").
// sessionApproveSet e.g. "agentsession:ns/name#approve";
// resourceOwnerSets e.g. ["repo:r1#owner"].
//
// The rule:
//   - resourceOwnerSets present ⇒ eligible = the UNION across the resource
//     owner-sets, deduped (any single owner of any source resource can
//     approve; quorum = 1). The session approve-set is NOT an operand.
//   - no resourceOwnerSets ⇒ eligible = the session approve-set (owner-only
//     gates: join, fork, metaagent, tool_call without a resource).
//
// RATIONALE — do NOT "harden" this into `session owners ∩ resource owners`.
// A resource-scoped approval has two parties: the requester, whose consent is
// implicit because their agent asked for the call, and the resource owner, the
// counterparty whose consent the gate exists to solicit. Requiring session
// standing as well collapses the pool to "people approving their own request" —
// the owner resolver writes only the requester as session owner, so the
// intersection is structurally EMPTY whenever requester ≠ resource owner, which
// is the only case an approval fires for. That has shipped as a production
// outage once, surfacing as "no one has standing to approve"; e2e missed it
// because fixtures seeded the resource owner as a session co-owner, a tuple no
// production path writes.
//
// Session standing is neither required (the owner vouches for THEIR resource,
// not for the session) nor sufficient (no self-approving access to someone
// else's resource). Across multiple resources the pool is the UNION, not the
// per-resource intersection. Click-time enforcement mirrors this rule in
// CheckApproverAuthorized; change both or neither.
func ResolveApprovers(ctx context.Context, l Lookuper, sessionApproveSet string, resourceOwnerSets []string) (approvers []string, empty bool, err error) {
	if len(resourceOwnerSets) == 0 {
		base, err := LookupApprovers(ctx, l, sessionApproveSet)
		if err != nil {
			return nil, false, err
		}
		return base, len(base) == 0, nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, rs := range resourceOwnerSets {
		members, err := LookupApprovers(ctx, l, rs)
		if err != nil {
			return nil, false, err
		}
		for _, s := range members {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out, len(out) == 0, nil
}
