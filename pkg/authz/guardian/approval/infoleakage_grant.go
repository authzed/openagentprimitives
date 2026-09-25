// WriteInfoLeakageGrants writes SpiceDB infoleakage_grant tuples on behalf of
// the runner's write-side info-leakage gate (T13). The grant shape mirrors the
// tool-approval grant pattern in pkg/authz/guardian/grants: a single batch
// WriteRelationships TOUCH call keyed by a randomly-generated grant ID.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// InfoLeakageGrantDefinition is the SpiceDB definition name for the
// infoleakage_grant resource type.
const InfoLeakageGrantDefinition = "infoleakage_grant"

// InfoLeakageResourceMatchCaveat is the name of the SpiceDB caveat that
// scopes an audience_subject relation to a specific (resource_type,
// resource_id) pair.
const InfoLeakageResourceMatchCaveat = "infoleakage_resource_match"

// LeakageSource identifies this package's info-leakage approval grant
// writes — the session and audience_subject tuples WriteInfoLeakageGrants
// produces on the fixed infoleakage_grant definition (InfoLeakageGrantDefinition).
// Both relations are literal and this package's writes are the only ones in
// tree; it does no deletes. Qualified (not plain Source) because this
// package owns more than one concept — see applied.go and orchestrator.go.
//
// Declared exactly once here; every wiring site (the runner, the e2e
// in-process runner factory) references this var rather than retyping the
// name, so the claim can never drift from what this writer actually
// presents.
var LeakageSource = relsource.Source{
	Name: "leakagegrants",
	Claims: []string{
		"infoleakage_grant#session",
		"infoleakage_grant#audience_subject",
	},
}

func init() {
	relsource.Register(LeakageSource)
}

// Writer is the minimal SpiceDB surface required to write infoleakage_grant
// tuples. The concrete spicedb.GrantWriter satisfies it; tests pass a
// recorder double.
//
// This duplicates the grants.Writer interface intentionally: the approval
// package does not import pkg/authz/guardian/grants (they are peers under
// pkg/authz/guardian), so we define the minimal surface locally rather than
// creating a cross-package dependency.
type Writer interface {
	// WriteRelationships issues one v1 WriteRelationships RPC carrying the
	// audience_subject tuples an approved share grants. An error means the
	// audience was NOT widened, so the caller must not report the share as
	// released.
	WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error)
}

// LeakageGrantResource is one accessed resource for which an
// audience_subject relation will be written. T13 builds this list from
// session-scope taint memory.
type LeakageGrantResource struct {
	Type string
	ID   string
}

// WriteInfoLeakageGrants writes one infoleakage_grant definition per call,
// identified by a randomly-generated grant ID. The relationships written are:
//
//   - infoleakage_grant:<grantID>#session@agentsession:<ns>/<name>  (one)
//   - infoleakage_grant:<grantID>#audience_subject@user:<subject>
//     with caveat infoleakage_resource_match{resource_type, resource_id}
//     and expiration = now+ttl  (one per audienceSubject × resource pair)
//
// All updates are sent in a single batched TOUCH call. The grant ID is
// returned so T13's gate-check path can correlate approvals with sessions.
func WriteInfoLeakageGrants(
	ctx context.Context,
	w Writer,
	sessionNamespace, sessionName string,
	audienceSubjects []string,
	resources []LeakageGrantResource,
	ttl time.Duration,
) (grantID string, err error) {
	if len(audienceSubjects) == 0 {
		return "", fmt.Errorf("WriteInfoLeakageGrants: audienceSubjects is empty (programming error)")
	}
	if len(resources) == 0 {
		return "", fmt.Errorf("WriteInfoLeakageGrants: resources is empty (programming error)")
	}

	grantID = newInfoLeakageGrantID()
	sessionObjectID := sessionNamespace + "/" + sessionName
	expiresAt := timestamppb.New(time.Now().Add(ttl))

	// Allocate the full update slice: 1 session + N*M audience_subject tuples.
	updates := make([]*v1.RelationshipUpdate, 0, 1+len(audienceSubjects)*len(resources))

	// Session relationship: ties the grant to the originating session.
	updates = append(updates, &v1.RelationshipUpdate{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{
				ObjectType: InfoLeakageGrantDefinition,
				ObjectId:   grantID,
			},
			Relation: "session",
			Subject: &v1.SubjectReference{
				Object: &v1.ObjectReference{
					ObjectType: "agentsession",
					ObjectId:   sessionObjectID,
				},
			},
		},
	})

	// One audience_subject relationship per (audienceSubject, resource) pair.
	// Each carries the infoleakage_resource_match caveat scoped to the exact
	// resource and expires after the caller-supplied TTL.
	for _, subject := range audienceSubjects {
		for _, res := range resources {
			cavCtx, err := structpb.NewStruct(map[string]any{
				"resource_type": res.Type,
				"resource_id":   res.ID,
			})
			if err != nil {
				return "", fmt.Errorf("build infoleakage_resource_match caveat ctx for subject %q resource %s/%s: %w",
					subject, res.Type, res.ID, err)
			}
			rel := &v1.Relationship{
				Resource: &v1.ObjectReference{
					ObjectType: InfoLeakageGrantDefinition,
					ObjectId:   grantID,
				},
				Relation: "audience_subject",
				Subject: &v1.SubjectReference{
					Object: &v1.ObjectReference{
						ObjectType: "user",
						ObjectId:   subject,
					},
				},
				OptionalCaveat: &v1.ContextualizedCaveat{
					CaveatName: InfoLeakageResourceMatchCaveat,
					Context:    cavCtx,
				},
				OptionalExpiresAt: expiresAt,
			}
			updates = append(updates, &v1.RelationshipUpdate{
				Operation:    v1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: rel,
			})
		}
	}

	if _, err := w.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: updates}); err != nil {
		return "", fmt.Errorf("WriteInfoLeakageGrants: write relationships: %w", err)
	}
	return grantID, nil
}

// newInfoLeakageGrantID returns a 32-character hex string from 16 random
// bytes, matching the request-ID generation pattern used elsewhere in the
// runner (pkg/agent/runner/loop.go: newRequestID).
func newInfoLeakageGrantID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("approval: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
