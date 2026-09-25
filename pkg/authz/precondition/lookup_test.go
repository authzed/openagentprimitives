package precondition_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
)

// TestLookupSubjectUsesTheRawIdVerbatim is the RULING P-2 contract, and it is
// worth pinning precisely because the correct implementation looks like it does
// nothing.
//
// Facts are written keyed by the RAW provider identifier. Two sides read them
// back — the bind-time filter that holds a candidate out of its slot, and the
// dispatch-time recomputation that explains the resulting denial — and they must
// name the same instance. Normalizing here (trimming, lowercasing, escaping)
// would make the lookup miss the value factcontent.Record actually wrote, and
// the miss fails as an EMPTY result: undetermined, forever, with nothing
// reporting a fault.
//
// So the ids that most look like they want cleaning up are exactly the ones this
// test insists come back untouched.
func TestLookupSubjectUsesTheRawIdVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		rawID string
	}{
		{
			name:  "a `#` that spicedb_escape would rewrite for the object id",
			rawID: "demo-org/demo-repo#6",
		},
		{
			name:  "mixed case a lowercase transform would fold",
			rawID: "Demo-Org/Demo-Repo#6",
		},
		{
			name:  "a URL a normalize_url transform would rewrite",
			rawID: "https://example.invalid/demo-org/demo-repo/pull/6",
		},
		{
			name:  "surrounding whitespace: Record wrote it, so the lookup must ask for it",
			rawID: " ba03f5969a ",
		},
		{
			name:  "a plain commit sha no chain would touch",
			rawID: "ba03f5969a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := precondition.LookupSubject("github_pr", tc.rawID)
			require.NoError(t, err)
			assert.Equal(t, "github_pr", got.ResourceType)
			assert.Equal(t, tc.rawID, got.ResourceID,
				"the fact-lookup id is the pre-transform value verbatim; anything else cannot match what Record wrote")
		})
	}
}

// An unnameable subject is an ERROR, not a lookup that quietly finds nothing.
// Both would fail closed, but an empty result is a legitimate verdict —
// undetermined — so an author would go hunting for an observation that never
// happened instead of the missing field that actually caused it.
func TestLookupSubjectRefusesAnUnnameableSubject(t *testing.T) {
	cases := []struct {
		name         string
		resourceType string
		rawID        string
		wantErr      string
	}{
		{
			name:  "no resource type",
			rawID: "ba03f5969a", wantErr: "names no resource type",
		},
		{
			name:         "no instance id",
			resourceType: "git_commit", wantErr: "names no instance id",
		},
		{
			name:    "neither",
			wantErr: "names no resource type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := precondition.LookupSubject(tc.resourceType, tc.rawID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, precondition.FactSubject{}, got,
				"a refused derivation must not hand back a half-built subject a caller could still query with")
		})
	}
}
