package spicedb

import (
	"context"
	"fmt"
	"io"
	"sort"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// EnsureAgentClassStarters makes agentclass:<ns>/<name>#starter hold EXACTLY
// subjects — each "user:<canonical>" or "<type>:<id>#<relation>" — touching
// the declared ones and deleting every other, in one WriteRelationships so a
// reader never observes a half-applied set.
//
// The class spec is the source of truth: a starter removed from
// spec.authz.session.allowedStarters must lose standing on the next reconcile,
// which is why this diffs against what SpiceDB holds rather than only touching.
// A malformed subject fails the whole call before any write.
//
// Reports a name SpiceDB cannot express as ErrUnrepresentableObjectID, for the
// same reason EnsureAgentClassPlatform does: the caller must stop retrying.
func (c *Client) EnsureAgentClassStarters(ctx context.Context, ns, name string, subjects []string) error {
	objectID, err := AgentClassObjectID(ns, name)
	if err != nil {
		return err
	}
	want := make(map[string]*v1.SubjectReference, len(subjects))
	for _, s := range subjects {
		objType, objID, rel, perr := parseSubjectRef(s)
		if perr != nil {
			return fmt.Errorf("agentclass#starter subject %q: %w", s, perr)
		}
		ref := &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: objType, ObjectId: objID}}
		if rel != "" {
			ref.OptionalRelation = rel
		}
		want[subjectKey(ref)] = ref
	}
	have, err := c.readAgentClassStarters(ctx, objectID)
	if err != nil {
		return err
	}
	var updates []*v1.RelationshipUpdate
	for key, ref := range want {
		if _, ok := have[key]; ok {
			continue // already present; nothing to touch
		}
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: starterRel(objectID, ref),
		})
	}
	for key, ref := range have {
		if _, ok := want[key]; ok {
			continue
		}
		updates = append(updates, &v1.RelationshipUpdate{
			Operation:    v1.RelationshipUpdate_OPERATION_DELETE,
			Relationship: starterRel(objectID, ref),
		})
	}
	if len(updates) == 0 {
		return nil
	}
	if _, err := c.cl.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates}); err != nil {
		return fmt.Errorf("write agentclass:%s#starter (%d updates): %w", objectID, len(updates), err)
	}
	return nil
}

// ListAgentClassStarters returns the subjects currently on
// agentclass:<ns>/<name>#starter, formatted as they are declared
// ("user:<id>", "group:<id>#member"), sorted. FullyConsistent, so a caller
// verifying a write it just made sees it.
func (c *Client) ListAgentClassStarters(ctx context.Context, ns, name string) ([]string, error) {
	objectID, err := AgentClassObjectID(ns, name)
	if err != nil {
		return nil, err
	}
	have, err := c.readAgentClassStarters(ctx, objectID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(have))
	for key := range have {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

// CheckAgentClassStart answers agentclass:<ns>/<name>#<permission> for
// user:<canonicalID>. permission is one of v1alpha1's
// AgentClassPermissionStartSession / AgentClassPermissionStartExplicit.
//
// ALWAYS FullyConsistent: the tuples are written by the AgentClass reconciler,
// possibly seconds before the session reconciler asks, and a MinimizeLatency
// read that missed them would refuse a legitimate starter — the same reasoning
// as LookupStartableClasses' gate path.
func (c *Client) CheckAgentClassStart(ctx context.Context, ns, name, permission string, canonicalID identity.CanonicalUserID) (bool, error) {
	objectID, err := AgentClassObjectID(ns, name)
	if err != nil {
		return false, err
	}
	return c.CheckOnResource(ctx, "agentclass", objectID, permission, canonicalID, true)
}

func (c *Client) readAgentClassStarters(ctx context.Context, objectID string) (map[string]*v1.SubjectReference, error) {
	stream, err := c.cl.ReadRelationships(ctx, &v1.ReadRelationshipsRequest{
		Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       "agentclass",
			OptionalResourceId: objectID,
			OptionalRelation:   "starter",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("read agentclass:%s#starter: %w", objectID, err)
	}
	have := map[string]*v1.SubjectReference{}
	for {
		resp, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read agentclass:%s#starter recv: %w", objectID, rerr)
		}
		ref := resp.GetRelationship().GetSubject()
		have[subjectKey(ref)] = ref
	}
	return have, nil
}

// subjectKey renders a subject reference in the declared form so the diff in
// EnsureAgentClassStarters compares like with like.
func subjectKey(ref *v1.SubjectReference) string {
	key := ref.GetObject().GetObjectType() + ":" + ref.GetObject().GetObjectId()
	if rel := ref.GetOptionalRelation(); rel != "" {
		key += "#" + rel
	}
	return key
}

func starterRel(objectID string, subject *v1.SubjectReference) *v1.Relationship {
	return &v1.Relationship{
		Resource: &v1.ObjectReference{ObjectType: "agentclass", ObjectId: objectID},
		Relation: "starter",
		Subject:  subject,
	}
}
