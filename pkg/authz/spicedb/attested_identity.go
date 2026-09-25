// attested_identity.go carries the tuple shapes that bind a provider-side
// account — a GitHub user, keyed by its numeric id — to the platform user
// whose credential catalog declared it.
//
// #user says who someone IS and confers no permission on its own — it is a
// relation on a type that exposes none (see the github kind's schema
// fragment) — and is written for EVERY binding, regardless of how many other
// subjects already claim the same account.
//
// #sole_user is different: it is written only while exactly one subject
// claims the account, and withdrawn the instant a second appears. Durable
// authority — repository roles, once a directory sync wires that consumer —
// traverses #sole_user, never #user. The derivation (recomputing the LIVE
// claimant count — from live UserIdentity CRs, not from stored #user tuples —
// every pass) lives in pkg/controllers/useridentity/attested_edge.go; this
// file only carries the write, the delete, and the lookup used to detect a
// withdrawal worth reporting.

package spicedb

import (
	"context"
	"errors"
	"fmt"
	"io"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// attestedIdentityRelation is the relation every attested-identity object type
// exposes. It is a RELATION, deliberately not a permission — see the type's own
// definition (pkg/authz/spicedb/schema/schema.zed's github_user).
const attestedIdentityRelation = "user"

// soleIdentityRelation is the relation written only while exactly one
// platform subject claims the account. Same object type, same schema
// definition, a different authorization posture: this is what durable
// authority is meant to traverse.
const soleIdentityRelation = "sole_user"

// TouchAttestedIdentity writes
//
//	<objType>:<subjectID>#user@user:<canonicalID>
//
// objType is the provider's SpiceDB object type ("github_user") and subjectID
// is the provider's own stable id for the account (GitHub's numeric user id as
// a string). Idempotent (TOUCH), so a reconciler that re-observes the same
// attestation converges instead of conflicting.
//
// The tuple is NOT exclusive: SpiceDB happily holds two subjects on one
// account, which is the intended behavior — two people who each link a
// credential for one shared account are both really there, and the platform
// records both and reports the collision rather than picking a winner.
func (c *Client) TouchAttestedIdentity(ctx context.Context, objType, subjectID string, canonicalID identity.CanonicalUserID) error {
	if objType == "" || subjectID == "" || canonicalID.String() == "" {
		return fmt.Errorf("touch attested identity: objType, subjectID and canonicalID are all required (got %q:%q@%q)",
			objType, subjectID, canonicalID.String())
	}
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: objType, ObjectId: subjectID},
				Relation: attestedIdentityRelation,
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()},
				},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch %s:%s#%s@%s: %w", objType, subjectID, attestedIdentityRelation,
			canonicalID.Subject().String(), err)
	}
	return nil
}

// LookupAttestedIdentitySubjects returns the canonical user ids currently bound
// to <objType>:<subjectID>#user — bare ids, not "user:<id>" refs, matching
// LookupSubjects.
//
// The read is FullyConsistent (inherited from LookupSubjects). That is the
// right freshness here and not merely tolerable: this is the read that decides
// whether a second person's claim on one account is a collision, and a stale
// answer would let the second claim land with no notice at all — the one
// signal the design offers a human.
func (c *Client) LookupAttestedIdentitySubjects(ctx context.Context, objType, subjectID string) ([]string, error) {
	if objType == "" || subjectID == "" {
		return nil, fmt.Errorf("lookup attested identity subjects: objType and subjectID are required (got %q:%q)", objType, subjectID)
	}
	return c.LookupSubjects(ctx, objType+":"+subjectID+"#"+attestedIdentityRelation)
}

// TouchSoleIdentity writes
//
//	<objType>:<subjectID>#sole_user@user:<canonicalID>
//
// Mirrors TouchAttestedIdentity's shape, but the relation it writes carries a
// different invariant: the caller (attested_edge.go) only reaches this once
// its own count of LIVE claimants on the account (from live UserIdentity CRs,
// not stored tuples) is exactly one, and calls DeleteSoleIdentity otherwise.
// TOUCH rather than CREATE because re-observing the same sole claimant on
// every reconcile pass must converge, not conflict — sole_user is derived,
// level-triggered state, recomputed every pass.
func (c *Client) TouchSoleIdentity(ctx context.Context, objType, subjectID string, canonicalID identity.CanonicalUserID) error {
	if objType == "" || subjectID == "" || canonicalID.String() == "" {
		return fmt.Errorf("touch sole identity: objType, subjectID and canonicalID are all required (got %q:%q@%q)",
			objType, subjectID, canonicalID.String())
	}
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: objType, ObjectId: subjectID},
				Relation: soleIdentityRelation,
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{ObjectType: "user", ObjectId: canonicalID.String()},
				},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch %s:%s#%s@%s: %w", objType, subjectID, soleIdentityRelation,
			canonicalID.Subject().String(), err)
	}
	return nil
}

// DeleteSoleIdentity withdraws <objType>:<subjectID>#sole_user, whoever
// currently holds it. The relation holds at most one subject by construction
// (attested_edge.go only ever writes it for a lone claimant), so the caller
// has no canonical id to name and this deletes by resource + relation alone.
//
// Called for every claimant once a second appears, and on every subsequent
// pass for as long as more than one claimant remains — recomputed, not
// event-driven, so a delete over an already-absent tuple must be (and is,
// via SpiceDB's own DeleteRelationships semantics) a harmless no-op rather
// than an error.
func (c *Client) DeleteSoleIdentity(ctx context.Context, objType, subjectID string) error {
	if objType == "" || subjectID == "" {
		return fmt.Errorf("delete sole identity: objType and subjectID are required (got %q:%q)", objType, subjectID)
	}
	if _, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       objType,
			OptionalResourceId: subjectID,
			OptionalRelation:   soleIdentityRelation,
		},
	}); err != nil {
		return fmt.Errorf("delete %s:%s#%s: %w", objType, subjectID, soleIdentityRelation, err)
	}
	return nil
}

// LookupSoleIdentitySubjects returns the canonical user ids currently holding
// <objType>:<subjectID>#sole_user — at most one, by construction (see
// TouchSoleIdentity/DeleteSoleIdentity). Bare ids, matching
// LookupAttestedIdentitySubjects's own shape.
//
// The caller (attested_edge.go) uses this to tell "this pass is the one
// taking sole_user away" from "it was already gone" — a withdrawal that
// LIVE cluster state (not this same relation) decided is now due. Without
// that distinction a withdrawal driven by a claimant whose own edge never
// reaches SpiceDB (its catalog never got far enough to write one) would
// either go unreported forever, or get re-reported on every single pass for
// as long as the condition persists; this makes it exactly one.
func (c *Client) LookupSoleIdentitySubjects(ctx context.Context, objType, subjectID string) ([]string, error) {
	if objType == "" || subjectID == "" {
		return nil, fmt.Errorf("lookup sole identity subjects: objType and subjectID are required (got %q:%q)", objType, subjectID)
	}
	return c.LookupSubjects(ctx, objType+":"+subjectID+"#"+soleIdentityRelation)
}

// LookupSoleIdentityAccounts is LookupSoleIdentitySubjects read from the other
// end: the provider-side account ids (objType resource ids) on which
// canonicalID currently holds #sole_user.
//
// It exists because the withdrawal side of this derivation cannot be driven
// from the catalog alone. attested_edge.go recomputes sole_user from the
// credentials a pass READ, so an account a catalog has stopped claiming — the
// credential removed, or the whole UserIdentity deleted — is simply never
// visited again, and the tuple it left behind keeps granting repository access
// with nothing left to revisit it. Asking the graph "what does this subject
// still hold?" is what makes the leftovers reachable at all.
//
// A relationship READ, not a LookupResources: sole_user is a relation, the
// answer needed is the exact stored tuples rather than anything the schema
// computes from them, and a withdrawal must never hinge on a permission
// expression a future schema edit could reroute.
//
// FullyConsistent for the same reason DeleteSoleIdentity's caller is
// fail-closed: a stale read here omits an account whose tuple then survives
// until something else happens to touch it, which for a deleted UserIdentity
// is never.
func (c *Client) LookupSoleIdentityAccounts(ctx context.Context, objType string, canonicalID identity.CanonicalUserID) ([]string, error) {
	if objType == "" || canonicalID.String() == "" {
		return nil, fmt.Errorf("lookup sole identity accounts: objType and canonicalID are required (got %q@%q)", objType, canonicalID.String())
	}
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:     objType,
			OptionalRelation: soleIdentityRelation,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "user",
				OptionalSubjectId: canonicalID.String(),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("read %s#%s@%s: %w", objType, soleIdentityRelation, canonicalID.Subject().String(), err)
	}
	var out []string
	for {
		msg, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read %s#%s@%s stream: %w", objType, soleIdentityRelation, canonicalID.Subject().String(), rerr)
		}
		out = append(out, msg.GetRelationship().GetResource().GetObjectId())
	}
	return out, nil
}
