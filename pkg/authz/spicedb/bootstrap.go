package spicedb

import (
	"context"
	"fmt"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// BootstrapSource identifies the SpiceDBBootstrap reconciler's writes — the
// same role TouchBootstrapRelationship/DeleteBootstrapRelationship below
// play as a convenience path over (*Client).Writer, and the one
// pkg/controllers/guardian binds a RelWriter to at its own construction
// site.
//
// Claims deliberately EMPTY. A SpiceDBBootstrap CR's spec.relationships is a
// free-form list — cluster-admin-declared Tuple{ResourceType, Relation,
// SubjectType, ...} values with no fixed set — the same unenumerable shape
// as an MCP toolspec's writesRelationships (see relwrites.Source). Claiming
// any relation here would either need to enumerate every relation any
// SpiceDBBootstrap CR anywhere might ever declare (impossible) or claim
// territory an admin has not actually asked to own.
//
// Exported and declared exactly once here — every wiring site (the operator,
// the e2e harness) references this var rather than retyping the name, so a
// claim can never drift from what a writer actually presents.
var BootstrapSource = relsource.Source{Name: "spicedbbootstrap"}

func init() {
	relsource.Register(BootstrapSource)
}

// Tuple is the controller-side representation of one SpiceDB
// relationship to TOUCH or DELETE for SpiceDBBootstrap. The fields are
// all required except SubjectRelation, which is empty for direct user
// subjects and set for subject-set references like "group:eng#member".
type Tuple struct {
	ResourceType    string
	ResourceID      string
	Relation        string
	SubjectType     string
	SubjectID       string
	SubjectRelation string // "" for direct subjects
}

// Key returns a stable comparable key for refcount-map lookups.
func (t Tuple) Key() string {
	return t.ResourceType + ":" + t.ResourceID + "#" + t.Relation +
		"@" + t.SubjectType + ":" + t.SubjectID + "#" + t.SubjectRelation
}

// BootstrapWriter is the minimal SpiceDB surface area used by the
// SpiceDBBootstrap reconciler. A RelWriter (from (*Client).Writer)
// satisfies it structurally — its WriteRelationships/DeleteRelationships
// are option-free by construction; the *GrantWriter adapter also satisfies
// it (option-stripped over a RelWriter); tests pass a recorder double.
//
// The shape mirrors guardian/grants.Writer (same proto types), but
// kept distinct to avoid a cross-package dependency from pkg/authz/spicedb
// onto pkg/authz/guardian/grants.
//
// Note that *authzed.Client itself does NOT satisfy this interface —
// its WriteRelationships/DeleteRelationships methods carry a variadic
// grpc.CallOption tail. RelWriter strips that tail before dispatch (see
// writer.go); convenience methods like TouchBootstrapRelationship use
// NewGrantWriter to do the same over a RelWriter.
type BootstrapWriter interface {
	// WriteRelationships issues one v1 WriteRelationships RPC. Bootstrap tuples
	// are written TOUCH, so re-running a bootstrap against an already-seeded
	// datastore converges instead of conflicting.
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)

	// DeleteRelationships issues one v1 DeleteRelationships RPC, used to retract
	// a bootstrap tuple whose source object is gone. An error means the tuple
	// may still exist and the grant it carries may still be live.
	DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error)
}

// TouchBootstrapRelationship TOUCHes one tuple using the receiver's
// underlying writer. Convenience method over TouchBootstrapRelationshipVia
// that uses a no-option adapter over c.Writer(BootstrapSource).
func (c *Client) TouchBootstrapRelationship(ctx context.Context, t Tuple) error {
	return TouchBootstrapRelationshipVia(ctx, NewGrantWriter(c.Writer(BootstrapSource)), t)
}

// DeleteBootstrapRelationship removes one tuple via the receiver's
// underlying writer.
func (c *Client) DeleteBootstrapRelationship(ctx context.Context, t Tuple) error {
	return DeleteBootstrapRelationshipVia(ctx, NewGrantWriter(c.Writer(BootstrapSource)), t)
}

// TouchBootstrapRelationshipVia is the package-level form: callers
// supply any BootstrapWriter (a *GrantWriter in production, a
// recorder in tests).
func TouchBootstrapRelationshipVia(ctx context.Context, w BootstrapWriter, t Tuple) error {
	subj := &v1.SubjectReference{
		Object: &v1.ObjectReference{ObjectType: t.SubjectType, ObjectId: t.SubjectID},
	}
	if t.SubjectRelation != "" {
		subj.OptionalRelation = t.SubjectRelation
	}
	_, err := w.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: t.ResourceType, ObjectId: t.ResourceID},
				Relation: t.Relation,
				Subject:  subj,
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("touch %s: %w", t.Key(), err)
	}
	return nil
}

// DeleteBootstrapRelationshipVia is the package-level form for delete.
func DeleteBootstrapRelationshipVia(ctx context.Context, w BootstrapWriter, t Tuple) error {
	sf := &v1.SubjectFilter{
		SubjectType:       t.SubjectType,
		OptionalSubjectId: t.SubjectID,
	}
	if t.SubjectRelation != "" {
		sf.OptionalRelation = &v1.SubjectFilter_RelationFilter{Relation: t.SubjectRelation}
	}
	_, err := w.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:          t.ResourceType,
			OptionalResourceId:    t.ResourceID,
			OptionalRelation:      t.Relation,
			OptionalSubjectFilter: sf,
		},
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", t.Key(), err)
	}
	return nil
}
