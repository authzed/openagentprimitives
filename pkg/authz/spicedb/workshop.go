package spicedb

import (
	"context"
	"fmt"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// EnsureWorkshopSubjects writes, in a single TOUCH WriteRelationships request,
// every tuple a workshop's standing rests on:
//
//   - workshop:<workshopID>#session@agentsession:<sessNS>/<sessName>
//   - workshop:<workshopID>#starter@user:<starter>   (omitted when unattributed)
//   - workshop:<workshopID>#platform@platform:platform
//
// Idempotent, so the controller re-ensures on every reconcile. One request, the
// same funnel TouchArtifactParent uses, because the three back two different
// permissions: `build` rests on #session, `close` on #starter + #platform. A
// workshop provisioned with only the first would refuse its own starter the
// close that frees their workshop slot — with no schema error, no failed
// reconcile and no log line. Keeping the write in one place is what makes that
// combination unrepresentable.
//
// starter is optional because Workshop.spec.starterCanonical is: a session
// carrying no started-by annotation leaves it empty. An empty subject id is
// InvalidArgument to SpiceDB and would fail the WHOLE request, taking #session
// with it, so an unattributed workshop would never reach Ready. Skipping the
// one tuple instead leaves it closable by a platform admin alone — fail closed
// on the permission, not on provisioning.
func (c *Client) EnsureWorkshopSubjects(ctx context.Context, workshopID, sessNS, sessName string, starter identity.CanonicalUserID) error {
	workshopRef := &v1.ObjectReference{ObjectType: "workshop", ObjectId: workshopID}
	touch := func(relation string, subject *v1.ObjectReference) *v1.RelationshipUpdate {
		return &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: workshopRef,
				Relation: relation,
				Subject:  &v1.SubjectReference{Object: subject},
			},
		}
	}
	updates := []*v1.RelationshipUpdate{
		touch("session", &v1.ObjectReference{ObjectType: "agentsession", ObjectId: sessNS + "/" + sessName}),
		touch("platform", &v1.ObjectReference{ObjectType: "platform", ObjectId: platformObjectID}),
	}
	if !starter.IsZero() {
		updates = append(updates, touch("starter", &v1.ObjectReference{ObjectType: "user", ObjectId: starter.String()}))
	}
	if _, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates}); err != nil {
		return fmt.Errorf("touch workshop:%s subjects (session/starter/platform): %w", workshopID, err)
	}
	return nil
}

// DeleteWorkshopRelationships removes every relation on the workshop object.
// Teardown MUST run this before the namespace goes away: a tuple that
// outlives its workshop would let a later namespace with the same name (a
// deliberately unreachable case given UID-derived names, but the invariant is
// cheap) inherit standing.
func (c *Client) DeleteWorkshopRelationships(ctx context.Context, workshopID string) error {
	_, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "workshop",
			OptionalResourceId: workshopID,
		},
	})
	if err != nil {
		return fmt.Errorf("delete workshop:%s relationships: %w", workshopID, err)
	}
	return nil
}

// CheckWorkshopBuild answers workshop:<workshopID>#build for the SESSION
// subject agentsession:<sessNS>/<sessName>. ALWAYS FullyConsistent: every
// caller is a privilege gate asking moments after a write (spec layer 1.3).
func (c *Client) CheckWorkshopBuild(ctx context.Context, workshopID, sessNS, sessName string) (bool, error) {
	resp, err := c.cl.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		Resource:    &v1.ObjectReference{ObjectType: "workshop", ObjectId: workshopID},
		Permission:  "build",
		Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
			ObjectType: "agentsession", ObjectId: sessNS + "/" + sessName,
		}},
	})
	if err != nil {
		return false, fmt.Errorf("check workshop:%s#build: %w", workshopID, err)
	}
	return resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
}

// CheckWorkshopClose answers workshop:<workshopID>#close for the PERSON
// subject user:<canonicalID> — may this human end this workshop from another
// builder? The subject is a user, not a session, on purpose: the permission is
// the starter's and a platform admin's, and a builder session asking on behalf
// of the person it acts for asks as that person.
//
// ALWAYS FullyConsistent, and deliberately without the `fullyConsistent bool`
// opt-in the neighbouring Check helpers carry — same reasoning as
// CheckWorkshopBuild, doubled by the write ordering. Both tuples this reads are
// written by the reconcile that provisions the TARGET workshop, so a
// MinimizeLatency snapshot can legitimately predate them. A stale read returns
// NO_PERMISSION, which the caller reports as "not yours to close" — a person
// refused their own workshop, with nothing in the schema, the tuples or the
// logs to show why. Making the consistency a parameter would let a future
// caller reintroduce that by passing false.
func (c *Client) CheckWorkshopClose(ctx context.Context, workshopID string, canonicalID identity.CanonicalUserID) (bool, error) {
	return c.checkUser(ctx, "workshop", workshopID, "close", canonicalID, consistencyFor(true),
		fmt.Sprintf("workshop:%s#close", workshopID))
}
