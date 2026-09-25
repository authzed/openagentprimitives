// Pure-Go unit tests for the attested-identity argument guards. They do NOT
// require a real SpiceDB endpoint and carry no build tag, so they run in every
// CI pass. The tuple-writing half is exercised against a live datastore by the
// integration suite; what is pinned here is that an incomplete attestation
// never reaches the wire at all.
package spicedb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// A zero-value Client has a nil embedded authzed client, so any call that
// reaches the wire panics. That is the point: these tests prove the guard
// returns FIRST. A guard that stopped running would not merely fail — it would
// crash, which is a louder signal than a wrong error string.
func TestTouchAttestedIdentity_RefusesAnIncompleteTuple(t *testing.T) {
	cases := []struct {
		name        string
		objType     string
		subjectID   string
		canonicalID identity.CanonicalUserID
	}{
		{name: "no object type: a tuple with no definition to land in", subjectID: "583231", canonicalID: identity.CanonicalFromTrusted("YWxpY2U", "test fixture")},
		{name: "no provider subject id: an account nobody named", objType: "github_user", canonicalID: identity.CanonicalFromTrusted("YWxpY2U", "test fixture")},
		{name: "no canonical user id: an identity edge binding nobody", objType: "github_user", subjectID: "583231"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Client{}).TouchAttestedIdentity(context.Background(), tc.objType, tc.subjectID, tc.canonicalID)
			require.Error(t, err, "an incomplete attestation must be refused, not written")
			assert.Contains(t, err.Error(), "touch attested identity",
				"the error names the operation that refused")
		})
	}
}

func TestLookupAttestedIdentitySubjects_RefusesAnIncompleteKey(t *testing.T) {
	cases := []struct {
		name      string
		objType   string
		subjectID string
	}{
		{name: "no object type: nothing to look under", subjectID: "583231"},
		{name: "no provider subject id: would read the whole definition", objType: "github_user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Client{}).LookupAttestedIdentitySubjects(context.Background(), tc.objType, tc.subjectID)
			require.Error(t, err, "a half-formed key must be refused, not read")
			assert.Nil(t, got, "a refused lookup returns no subjects; an empty slice would read as 'nobody is bound'")
			assert.Contains(t, err.Error(), "lookup attested identity subjects",
				"the error names the operation that refused")
		})
	}
}

// Same guard, same reason, for the #sole_user pair: a zero-value Client
// proves the argument check runs before anything reaches the wire.
func TestTouchSoleIdentity_RefusesAnIncompleteTuple(t *testing.T) {
	cases := []struct {
		name        string
		objType     string
		subjectID   string
		canonicalID identity.CanonicalUserID
	}{
		{name: "no object type: a tuple with no definition to land in", subjectID: "583231", canonicalID: identity.CanonicalFromTrusted("YWxpY2U", "test fixture")},
		{name: "no provider subject id: an account nobody named", objType: "github_user", canonicalID: identity.CanonicalFromTrusted("YWxpY2U", "test fixture")},
		{name: "no canonical user id: an identity edge binding nobody", objType: "github_user", subjectID: "583231"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Client{}).TouchSoleIdentity(context.Background(), tc.objType, tc.subjectID, tc.canonicalID)
			require.Error(t, err, "an incomplete sole-identity write must be refused, not written")
			assert.Contains(t, err.Error(), "touch sole identity",
				"the error names the operation that refused")
		})
	}
}

func TestDeleteSoleIdentity_RefusesAnIncompleteKey(t *testing.T) {
	cases := []struct {
		name      string
		objType   string
		subjectID string
	}{
		{name: "no object type: nothing to delete under", subjectID: "583231"},
		{name: "no provider subject id: would delete across the whole definition", objType: "github_user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Client{}).DeleteSoleIdentity(context.Background(), tc.objType, tc.subjectID)
			require.Error(t, err, "a half-formed key must be refused, not deleted")
			assert.Contains(t, err.Error(), "delete sole identity",
				"the error names the operation that refused")
		})
	}
}

func TestLookupSoleIdentitySubjects_RefusesAnIncompleteKey(t *testing.T) {
	cases := []struct {
		name      string
		objType   string
		subjectID string
	}{
		{name: "no object type: nothing to look under", subjectID: "583231"},
		{name: "no provider subject id: would read the whole definition", objType: "github_user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Client{}).LookupSoleIdentitySubjects(context.Background(), tc.objType, tc.subjectID)
			require.Error(t, err, "a half-formed key must be refused, not read")
			assert.Nil(t, got, "a refused lookup returns no subjects; an empty slice would read as 'nobody holds it'")
			assert.Contains(t, err.Error(), "lookup sole identity subjects",
				"the error names the operation that refused")
		})
	}
}
