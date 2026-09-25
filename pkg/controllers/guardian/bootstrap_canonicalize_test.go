package guardian_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestResolveTuple_DirectUser_Canonicalized(t *testing.T) {
	rel := spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "group", ID: "engineering"},
		Relation: "member",
		Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
			Type: "user", ID: "Alice@Example.com", Canonicalize: true,
		},
	}
	tup := guardian.ResolveTuple(rel)
	want, err := identity.EmailReference("Alice@Example.com").Canonical()
	require.NoError(t, err)
	assert.Equal(t, "group", tup.ResourceType)
	assert.Equal(t, "engineering", tup.ResourceID)
	assert.Equal(t, "member", tup.Relation)
	assert.Equal(t, "user", tup.SubjectType)
	// identity boundary: asserts the string Tuple.SubjectID.
	assert.Equal(t, want.String(), tup.SubjectID)
	assert.Equal(t, "", tup.SubjectRelation)
}

func TestResolveTuple_SubjectSet(t *testing.T) {
	rel := spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "agentsession", ID: "default/foo"},
		Relation: "participant",
		Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
			Type: "group", ID: "eng", Relation: "member",
		},
	}
	tup := guardian.ResolveTuple(rel)
	assert.Equal(t, "group", tup.SubjectType)
	assert.Equal(t, "eng", tup.SubjectID)
	assert.Equal(t, "member", tup.SubjectRelation)
}

func TestResolveTuple_DirectUser_NoCanonicalize(t *testing.T) {
	rel := spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "group", ID: "engineering"},
		Relation: "member",
		Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
			Type: "user", ID: "already-canonical-id",
		},
	}
	tup := guardian.ResolveTuple(rel)
	assert.Equal(t, "already-canonical-id", tup.SubjectID)
}

func TestResolveTuple_Wildcard(t *testing.T) {
	rel := spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "crm_company", ID: "123"},
		Relation: "any_user",
		Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
			Type: "user", Wildcard: true,
		},
	}
	tup := guardian.ResolveTuple(rel)
	assert.Equal(t, "user", tup.SubjectType)
	assert.Equal(t, "*", tup.SubjectID, "wildcard subject must serialize as \"*\"")
	assert.Equal(t, "", tup.SubjectRelation)
}

func TestResolveTuple_Wildcard_BeatsCanonicalize(t *testing.T) {
	// Validation rejects this combo, but if both are set, wildcard wins.
	rel := spiceboxv1alpha1.SpiceDBBootstrapRelationship{
		Resource: spiceboxv1alpha1.SpiceDBObjectRef{Type: "crm_company", ID: "123"},
		Relation: "any_user",
		Subject: spiceboxv1alpha1.SpiceDBSubjectRef{
			Type: "user", ID: "alice@example.com", Wildcard: true, Canonicalize: true,
		},
	}
	tup := guardian.ResolveTuple(rel)
	assert.Equal(t, "*", tup.SubjectID)
}
