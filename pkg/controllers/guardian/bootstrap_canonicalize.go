package guardian

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ResolveTuple converts a SpiceDBBootstrapRelationship into the
// spicedb.Tuple form the writer accepts.
//
//   - subject.wildcard=true forces SubjectID to "*" (the SpiceDB
//     wildcard-subject wire form). Wins over canonicalize.
//   - subject.canonicalize=true replaces ID with
//     identity.EmailReference(id).Canonical().
//
// Validation that wildcard/canonicalize are only legal in valid
// combinations lives in ValidateSpec; this helper assumes the spec
// has already passed validation.
func ResolveTuple(rel spiceboxv1alpha1.SpiceDBBootstrapRelationship) spicedb.Tuple {
	subjectID := rel.Subject.ID
	switch {
	case rel.Subject.Wildcard:
		subjectID = "*"
	case rel.Subject.Canonicalize:
		// Canonicalize replaces the (email) ID with its base64 canonical. A
		// valid email never errors; an empty/invalid ID — a misconfig
		// ValidateSpec rejects — leaves subjectID as the raw ID so the
		// downstream SpiceDB write fails loudly rather than minting a phantom.
		if c, cerr := identity.EmailReference(identity.Email(rel.Subject.ID)).Canonical(); cerr == nil {
			// identity boundary: spicedb.Tuple.SubjectID is a string (SpiceDB wire); the canonical is serialized here.
			subjectID = c.String()
		}
	}
	return spicedb.Tuple{
		ResourceType:    rel.Resource.Type,
		ResourceID:      rel.Resource.ID,
		Relation:        rel.Relation,
		SubjectType:     rel.Subject.Type,
		SubjectID:       subjectID,
		SubjectRelation: rel.Subject.Relation,
	}
}
