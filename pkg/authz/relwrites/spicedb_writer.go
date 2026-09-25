// Package relwrites — SpiceDB-backed Writer adapter.
//
// SpiceDBWriter implements the package's Writer interface by translating
// []ResolvedTuple into a single batched WriteRelationships RPC against
// SpiceDB v1's PermissionsService. The translation:
//   - Splits each "<type>:<id>" string into ObjectReference{ObjectType, ObjectId}.
//   - Emits all updates as OPERATION_TOUCH so repeated post-effect writes
//     are idempotent — a tool that fires twice with the same args produces
//     the same tuples both times and the second write is a no-op in SpiceDB.
//
// Same minimal-Writer-subset pattern as pkg/authz/guardian/grants.Writer: the
// interface is declared locally so tests can substitute a recorder
// without pulling the full v1.PermissionsServiceClient gRPC mock.
package relwrites

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// Source identifies this package's JIT post-effect relationship writes —
// the tuples a toolspec's writesRelationships declares.
//
// Claims deliberately EMPTY, and that is the point rather than an omission:
// this writer resolves whatever resource#relation@subject a TOOLSPEC's
// writesRelationships names (via CEL, at call time), so it owns nothing —
// it is the mechanism, not an owner. Leaving it unclaimed means a toolspec
// author who declares a write to a relation another source owns is refused
// BY CONSTRUCTION, at the write, rather than only being caught by review.
// Claiming anything here would do the opposite: it would let this writer
// bypass the very protection its own unclaimed status provides everyone
// else.
//
// Declared exactly once here; every wiring site (the runner, the e2e
// in-process runner factory, the SRE pinning producer) references this var
// rather than retyping the name, so the (lack of) claim can never drift from
// what this writer actually presents.
var Source = relsource.Source{Name: "relwrites"}

func init() {
	relsource.Register(Source)
}

// ErrWriteOnceConflict is returned by SpiceDBWriter.WriteRelationships when a
// tuple's Exclusive MUST_NOT_MATCH precondition matched — i.e. the subject
// already holds the relation on some resource of that type, so the write is
// atomically rejected and NOTHING was written. Distinguishable (errors.Is-able)
// from a hard RPC error so callers can treat it as the intended "second pin
// rejected" signal rather than a failure. See the sandbox tool's
// evaluateWritesRelationships (it logs this at INFO, not as an error).
var ErrWriteOnceConflict = errors.New("relwrites: exclusive write-once conflict (subject already holds this relation)")

// SpiceDBClient is the subset of v1.PermissionsServiceClient required by
// SpiceDBWriter. The concrete authzed v1 client satisfies it; tests use
// a recorder double.
type SpiceDBClient interface {
	// WriteRelationships issues one v1 WriteRelationships RPC. Preconditions on
	// the request are honored by SpiceDB atomically, so a FAILED_PRECONDITION
	// error means NOTHING was written — SpiceDBWriter maps that case to
	// ErrWriteOnceConflict rather than a generic failure.
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)
}

// SpiceDBWriter is the production Writer binding for relwrites.Run. It
// batches all resolved tuples for a single block into one RPC and uses
// TOUCH so repeats are idempotent.
type SpiceDBWriter struct {
	Client SpiceDBClient
}

// WriteRelationships TOUCHes every tuple in a single batched RPC.
// Returns an error if any tuple's resource or subject is not of the
// form "<type>:<id>", or if the underlying RPC fails. An empty input
// is a no-op and returns nil — no RPC is issued.
//
// For any tuple with Exclusive == true, a MUST_NOT_MATCH precondition is
// attached to the same request: the write is admitted only if the subject
// does NOT already hold `relation` on any resource of the tuple's resource
// type (resource id left unset so it matches ANY resource). Because all
// preconditions ride the one WriteRelationshipsRequest, a matching
// precondition makes SpiceDB write NOTHING (atomic) and return
// FAILED_PRECONDITION — which this method maps to the distinguishable
// ErrWriteOnceConflict sentinel (the "second pin rejected" signal). Any other
// RPC error is returned verbatim.
func (s *SpiceDBWriter) WriteRelationships(ctx context.Context, tuples []ResolvedTuple) error {
	if len(tuples) == 0 {
		return nil
	}
	updates := make([]*v1.RelationshipUpdate, 0, len(tuples))
	var preconds []*v1.Precondition
	var exclusiveTuples []ResolvedTuple
	for _, t := range tuples {
		rt, rid, err := splitObject(t.Resource)
		if err != nil {
			return fmt.Errorf("relwrites: bad resource: %w", err)
		}
		st, sid, err := splitObject(t.Subject)
		if err != nil {
			return fmt.Errorf("relwrites: bad subject: %w", err)
		}
		updates = append(updates, &v1.RelationshipUpdate{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: rt, ObjectId: rid},
				Relation: t.Relation,
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: st, ObjectId: sid}},
			},
		})
		if t.Exclusive {
			// resource id UNSET: the subject may hold `relation` on NO resource
			// of this type. A match ⇒ already pinned ⇒ atomic rejection.
			preconds = append(preconds, &v1.Precondition{
				Operation: v1.Precondition_OPERATION_MUST_NOT_MATCH,
				Filter: &v1.RelationshipFilter{
					ResourceType:     rt,
					OptionalRelation: t.Relation,
					OptionalSubjectFilter: &v1.SubjectFilter{
						SubjectType:       st,
						OptionalSubjectId: sid,
					},
				},
			})
			exclusiveTuples = append(exclusiveTuples, t)
		}
	}
	_, err := s.Client.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates:               updates,
		OptionalPreconditions: preconds,
	})
	if err != nil {
		if len(preconds) > 0 && status.Code(err) == codes.FailedPrecondition {
			// The MUST_NOT_MATCH precondition matched — subject already pinned.
			// SpiceDB wrote nothing; surface the distinguishable sentinel with
			// the offending tuple(s) for operator context.
			return fmt.Errorf("%w: %s", ErrWriteOnceConflict, formatExclusiveTuples(exclusiveTuples))
		}
		return err
	}
	return nil
}

// splitObject splits a "<type>:<id>" object reference into its two parts.
// Inlined from pkg/authz/spicedb.SplitObject (identical logic) rather than
// imported: that package's client.go also imports
// pkg/platform/identity/externaltoken for its live-client constructor, and
// externaltoken imports pkg/apis/v1alpha1 -- so importing the package for
// this one pure string-split pulled pkg/apis/v1alpha1 into every consumer of
// this package's compile-only surface (pkg/authz/observe, and anything that
// imports it) transitively, which is a real import cycle for any package
// v1alpha1 itself depends on (e.g. pkg/tools/toolspec/toolkit, via
// SpiceboxToolkitSpec.ToToolkit()'s return type). WriteRelationships is the
// one place in this package that ever needed spicedb at all; nothing else
// in the file, or the package, does.
func splitObject(s string) (objType, objID string, err error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("object %q: must be of the form type:id", s)
	}
	return s[:i], s[i+1:], nil
}

// formatExclusiveTuples renders the exclusive tuple(s) of a rejected batch as
// "resource#relation@subject" for the conflict error's context. No values
// (only CEL-resolved object refs) are involved, so this is safe to include.
func formatExclusiveTuples(tuples []ResolvedTuple) string {
	if len(tuples) == 1 {
		t := tuples[0]
		return fmt.Sprintf("%s#%s@%s", t.Resource, t.Relation, t.Subject)
	}
	parts := make([]string, 0, len(tuples))
	for _, t := range tuples {
		parts = append(parts, fmt.Sprintf("%s#%s@%s", t.Resource, t.Relation, t.Subject))
	}
	return fmt.Sprintf("%v", parts)
}
