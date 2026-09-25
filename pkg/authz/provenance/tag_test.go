package provenance_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

func TestNewLeafIsALeafAndNeverCarriesDerivedFrom(t *testing.T) {
	tag, err := provenance.NewLeaf("pt-1", []string{"user:alice", "user:bob"}, false)
	require.NoError(t, err)

	assert.Equal(t, provenance.TagID("pt-1"), tag.ID())
	assert.True(t, tag.IsLeaf())
	assert.False(t, tag.IsDerived())
	assert.Equal(t, []string{"user:alice", "user:bob"}, tag.DirectReaders())
	assert.Empty(t, tag.DerivedFrom(),
		"a leaf with derivation edges would be read through BOTH arms of `reader = direct_reader + derived_from.all(reader)`, and the + is a union")
}

func TestNewDerivedIsDerivedAndNeverCarriesDirectReaders(t *testing.T) {
	tag, err := provenance.NewDerived("pt-3", []provenance.TagID{"pt-1", "pt-2"})
	require.NoError(t, err)

	assert.Equal(t, provenance.TagID("pt-3"), tag.ID())
	assert.True(t, tag.IsDerived())
	assert.False(t, tag.IsLeaf())
	assert.Equal(t, []provenance.TagID{"pt-1", "pt-2"}, tag.DerivedFrom())
	assert.Empty(t, tag.DirectReaders(),
		"THE invariant: a derived tag carrying its own direct readers widens past the intersection, because `reader` unions the two arms — it would be readable by someone authorized on none of its sources")
}

// TestALeafWithNoReadersIsValidAndUndisclosable pins a case that looks like an
// error and is not.
//
// LookupSubjects over a resource nobody can read legitimately returns nothing.
// The resulting leaf is readable by nobody, which is the correct and
// fail-closed answer for a datum with no audience. Rejecting it at
// construction would push the mint hook into either dropping the datum
// silently (untracked, and then invisible to the egress check) or inventing an
// audience it was not given.
func TestALeafWithNoReadersIsValidAndUndisclosable(t *testing.T) {
	tag, err := provenance.NewLeaf("pt-lonely", nil, false)
	require.NoError(t, err, "a resource with no authorized readers is a real case, not a malformed one")
	assert.True(t, tag.IsLeaf())
	assert.Empty(t, tag.DirectReaders())
}

func TestConstructorsRejectTheStatesThatCannotBeMeaningful(t *testing.T) {
	cases := []struct {
		name    string
		build   func() error
		wantErr string
	}{
		{
			name:    "leaf with no id",
			build:   func() error { _, err := provenance.NewLeaf("", []string{"user:alice"}, false); return err },
			wantErr: "id",
		},
		{
			name:    "derived with no id",
			build:   func() error { _, err := provenance.NewDerived("", []provenance.TagID{"pt-1"}); return err },
			wantErr: "id",
		},
		{
			// Distinct from the no-readers leaf above: that one is readable by
			// nobody because its SOURCE has no audience, which is a fact. This
			// one claims to be derived from nothing, which is not a fact about
			// any data — it is a caller bug, and it would silently produce an
			// undisclosable tag that looks deliberate.
			name:    "derived from nothing",
			build:   func() error { _, err := provenance.NewDerived("pt-3", nil); return err },
			wantErr: "at least one source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestZeroTagIsNeitherLeafNorDerived pins that the zero value is inert rather
// than silently leaf-shaped.
//
// The constructors are the only way to obtain a usable Tag, but a zero Tag can
// still appear as a struct field or a map miss. It must answer false to both
// questions so a consumer branching on IsLeaf/IsDerived cannot be handed one
// and treat it as an empty-reader leaf — which would be an undisclosable datum
// masquerading as a checked one.
func TestZeroTagIsNeitherLeafNorDerived(t *testing.T) {
	var zero provenance.Tag
	assert.False(t, zero.IsLeaf())
	assert.False(t, zero.IsDerived())
}

// TestAccessorsCopy stops a caller from editing a tag's audience after
// construction. An authorization input that a consumer can mutate in place is
// not an input, it is a suggestion.
func TestAccessorsCopy(t *testing.T) {
	tag, err := provenance.NewLeaf("pt-1", []string{"user:alice"}, false)
	require.NoError(t, err)
	got := tag.DirectReaders()
	got[0] = "user:mallory"
	assert.Equal(t, []string{"user:alice"}, tag.DirectReaders(),
		"mutating a returned slice must not rewrite the tag's audience")

	derived, err := provenance.NewDerived("pt-2", []provenance.TagID{"pt-1"})
	require.NoError(t, err)
	srcs := derived.DerivedFrom()
	srcs[0] = "pt-evil"
	assert.Equal(t, []provenance.TagID{"pt-1"}, derived.DerivedFrom(),
		"mutating a returned slice must not rewrite the derivation tree")
}

// TestUntrustedOriginIsALeafSelfMark pins that integrity is declared where the
// datum ENTERS the system and inherited everywhere below.
//
// `carries_untrusted = untrusted_origin + derived_from->carries_untrusted`, so
// a derived tag gets its answer from its sources by construction. Letting a
// derived tag declare its own origin mark would let an assembling agent stamp
// a combination as trusted, which is laundering in the integrity direction.
func TestUntrustedOriginIsALeafSelfMark(t *testing.T) {
	untrusted, err := provenance.NewLeaf("pt-web", []string{"user:alice"}, true)
	require.NoError(t, err)
	assert.True(t, untrusted.UntrustedOrigin())

	trusted, err := provenance.NewLeaf("pt-db", []string{"user:alice"}, false)
	require.NoError(t, err)
	assert.False(t, trusted.UntrustedOrigin())

	derived, err := provenance.NewDerived("pt-mix", []provenance.TagID{"pt-web", "pt-db"})
	require.NoError(t, err)
	assert.False(t, derived.UntrustedOrigin(),
		"a derived tag carries no origin mark of its own; carries_untrusted resolves through derived_from in SpiceDB")
}
