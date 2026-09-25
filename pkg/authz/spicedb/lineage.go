package spicedb

import (
	"context"
	"fmt"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Delegation lineage. The edge between a delegating session and the child it
// spawned is recorded as TWO tuples written together:
//
//	agentsession:<child>#parent@agentsession:<parent>   (upward)
//	agentsession:<parent>#child@agentsession:<child>    (downward)
//
// Standing flows only along the upward one — approve, hold and read_transcript
// arrow through `parent`, so revoking on the parent takes effect on every
// descendant at the next check rather than being copied onto the child. The
// downward tuple carries no arrow at all; it exists solely so `converse` can
// answer the child->parent direction, which no expression rooted at the parent
// could otherwise reach (see the `child` relation's comment in schema.zed).
//
// TouchLineage writes both in ONE WriteRelationships call, which SpiceDB
// applies atomically, so the pair can never half-land and leave a delegation
// that is conversational in one direction only. Idempotent (TOUCH), so a
// retried reconcile is a no-op.
func (c *Client) TouchLineage(ctx context.Context, childNS, childName, parentNS, parentName string) error {
	childObj := &v1.ObjectReference{ObjectType: "agentsession", ObjectId: childNS + "/" + childName}
	parentObj := &v1.ObjectReference{ObjectType: "agentsession", ObjectId: parentNS + "/" + parentName}
	_, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{
			{
				Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &v1.Relationship{
					Resource: childObj, Relation: "parent",
					Subject: &v1.SubjectReference{Object: parentObj},
				},
			},
			{
				Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &v1.Relationship{
					Resource: parentObj, Relation: "child",
					Subject: &v1.SubjectReference{Object: childObj},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("touch agentsession lineage (#parent + #child): %w", err)
	}
	return nil
}

// CheckConverse answers
//
//	agentsession:<ns>/<name>#converse@agentsession:<senderNS>/<senderName>
//
// — may the SENDING session put a turn into the named one. This is the
// agent-to-agent counterpart of CheckInteract, and it is a separate method
// rather than a widened CheckInteract precisely because the two take different
// SpiceDB subject TYPES: checkUser would build `user:agentsession:<ns>/<name>`,
// which SpiceDB rejects outright with InvalidArgument — an object id may not
// contain ':' — so the question is not merely unanswerable, it is unaskable
// (pinned by TestInteract_RejectsASessionReferenceSmuggledInAsAUserID).
//
// Callers MUST bind fullyConsistent=true: the lineage tuples are written by the
// SubagentRequest controller in the same pass that creates the child, so the
// child's very first message can race a MinimizeLatency read and be refused.
//
// Returns (false, err) on any RPC failure; every caller denies (fail-CLOSED).
func (c *Client) CheckConverse(ctx context.Context, ns, name, senderNS, senderName string, fullyConsistent bool) (bool, error) {
	return c.check(ctx, &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + name},
		Permission: "converse",
		Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
			ObjectType: "agentsession", ObjectId: senderNS + "/" + senderName,
		}},
		Consistency: consistencyFor(fullyConsistent),
	}, "converse")
}

// CheckReadTranscript checks agentsession:<ns>/<name>#read_transcript@user:<canonicalID>.
// This is the gate memory_entry#read resolves through; for a root session it is
// identical to interact.
func (c *Client) CheckReadTranscript(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return c.checkUser(ctx, "agentsession", ns+"/"+name, "read_transcript", canonicalID, consistencyFor(fullyConsistent), "read_transcript")
}

// CheckReadTranscriptForSession answers
//
//	agentsession:<childNS>/<childName>#read_transcript@agentsession:<subjectNS>/<subjectName>
//
// — the SESSION-typed counterpart of CheckReadTranscript, which only ever
// builds a "user:<id>" subject (see CheckConverse's comment on why an
// agentsession-typed subject is unaskable through checkUser). This answers
// the `+ parent` arm the schema gained in plan 4a task 3: the workshop
// transcript route (pkg/web/workshoptranscriptsrv) uses it to let a builder
// session read the transcript of a child it spawned to test what it built,
// with no human in the loop.
//
// ALWAYS FullyConsistent, mirroring CheckWorkshopBuild: every caller of this
// method is a privilege gate the route asks moments after resolving the
// caller's own Workshop and re-deriving the parent from it — never a
// steady-state read with room to tolerate a stale replica.
//
// Returns (false, err) on any RPC failure; every caller denies (fail-CLOSED).
func (c *Client) CheckReadTranscriptForSession(ctx context.Context, childNS, childName, subjectNS, subjectName string) (bool, error) {
	return c.check(ctx, &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: "agentsession", ObjectId: childNS + "/" + childName},
		Permission: "read_transcript",
		Subject: &v1.SubjectReference{Object: &v1.ObjectReference{
			ObjectType: "agentsession", ObjectId: subjectNS + "/" + subjectName,
		}},
		Consistency: consistencyFor(true),
	}, "read_transcript")
}

// DeleteLineage removes BOTH halves of the delegation edge — the child's
// upward `parent` tuple and the parent's downward `child` tuple. Deleting only
// one would leave a session conversational in a single direction, which is not
// a state any writer can produce.
//
// It has no caller today: the AgentSession finalizer's
// DeleteAgentSessionRelationships filters on agentsession as the RESOURCE, so
// reaping the child already removes the upward tuple and reaping the parent
// already removes the downward one. DeleteLineage exists for the narrow case of
// re-parenting, which nothing does.
//
// Note the consequence of that resource-side filter: reaping ONLY the child
// leaves the parent's `#child` tuple naming a session that no longer exists.
// It grants nothing — a deleted session publishes nothing — and it is cleared
// when the parent itself is reaped.
func (c *Client) DeleteLineage(ctx context.Context, childNS, childName, parentNS, parentName string) error {
	if _, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: childNS + "/" + childName,
			OptionalRelation:   "parent",
		},
	}); err != nil {
		return fmt.Errorf("delete agentsession#parent: %w", err)
	}
	if _, err := c.cl.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentsession",
			OptionalResourceId: parentNS + "/" + parentName,
			OptionalRelation:   "child",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "agentsession",
				OptionalSubjectId: childNS + "/" + childName,
			},
		},
	}); err != nil {
		return fmt.Errorf("delete agentsession#child: %w", err)
	}
	return nil
}
