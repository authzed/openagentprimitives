package pttag_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
)

func TestRoundTripPreservesALeaf(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	leaf, err := provenance.NewLeaf("ptt-1", []string{"user:alice"}, true)
	require.NoError(t, err)

	rec := pttag.FromTag(leaf, []string{"doc:d1"}, "toolu_7", now)
	assert.Equal(t, pttag.KindLeaf, rec.Kind)
	assert.Equal(t, "toolu_7", rec.ToolUseID)
	assert.Equal(t, now, rec.MintedAt)

	got, err := rec.ToTag()
	require.NoError(t, err)
	assert.True(t, got.IsLeaf())
	assert.Equal(t, []string{"user:alice"}, got.DirectReaders())
	assert.True(t, got.UntrustedOrigin())
}

func TestRoundTripPreservesADerivedTag(t *testing.T) {
	derived, err := provenance.NewDerived("ptt-3", []provenance.TagID{"ptt-1", "ptt-2"})
	require.NoError(t, err)

	rec := pttag.FromTag(derived, nil, "toolu_9", time.Unix(0, 0).UTC())
	assert.Equal(t, pttag.KindDerived, rec.Kind)
	assert.Empty(t, rec.DirectReaders)

	got, err := rec.ToTag()
	require.NoError(t, err)
	assert.True(t, got.IsDerived())
	assert.Equal(t, []provenance.TagID{"ptt-1", "ptt-2"}, got.DerivedFrom())
}

// TestToTagRefusesRecordsThatBreakTheInvariant is the reason ToTag routes
// through the constructors instead of populating a struct.
//
// A stored record is data, and data can be wrong: truncated, hand-edited, or
// written by a build that predates a rule. If deserialization trusted it, a
// record carrying BOTH direct readers and derivation edges would load into a
// tag read through both arms of `reader = direct_reader + derived_from.all(reader)`
// — resolving wider than any of its sources permit, with nothing to notice.
//
// Every case here is refused rather than repaired. Silently dropping the
// offending half would pick an audience nobody wrote down.
func TestToTagRefusesRecordsThatBreakTheInvariant(t *testing.T) {
	cases := []struct {
		name    string
		rec     pttag.TagRecord
		wantErr string
	}{
		{
			name: "leaf carrying derivation edges",
			rec: pttag.TagRecord{
				ID: "ptt-x", Kind: pttag.KindLeaf,
				DirectReaders: []string{"user:alice"},
				DerivedFrom:   []string{"ptt-1"},
			},
			wantErr: "both arms",
		},
		{
			name: "derived carrying its own readers",
			rec: pttag.TagRecord{
				ID: "ptt-y", Kind: pttag.KindDerived,
				DerivedFrom:   []string{"ptt-1"},
				DirectReaders: []string{"user:mallory"},
			},
			wantErr: "widens past the intersection",
		},
		{
			name:    "unknown kind",
			rec:     pttag.TagRecord{ID: "ptt-z", Kind: "whatever"},
			wantErr: "unknown kind",
		},
		{
			name:    "derived from nothing",
			rec:     pttag.TagRecord{ID: "ptt-w", Kind: pttag.KindDerived},
			wantErr: "at least one source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.rec.ToTag()
			require.Error(t, err, "a malformed record must be refused, never loaded")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRetentionIsAppendOnly pins that a tag's audience cannot be rewritten
// after a disclosure was checked against it. A mutable record would let a
// later write widen what an earlier check saw, with the audit trail agreeing.
func TestRetentionIsAppendOnly(t *testing.T) {
	assert.True(t, pttag.Kind{}.Retention().AppendOnly,
		"an authorization input that can be edited after the fact is not evidence of anything")
}
