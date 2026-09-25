package pttagmint_test

// What a tag was minted FROM.
//
// The reader set says WHO may see a datum and never WHY. Two things need the
// why: an audit reconstructing how an audience was arrived at, and any routing
// that must find a human able to speak for the datum — `pt_tag` has no owner
// relation of its own, so the only route to one is back through the objects it
// came from.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
)

func TestALeafRecordsWhatItWasMintedFrom(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{
		"doc:d1#viewer": {"tim"},
		"doc:d2#viewer": {"tim"},
	}}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{
			{Type: "doc", ID: "d1", Permission: "viewer"},
			{Type: "doc", ID: "d2", Permission: "viewer"},
		},
	})
	require.NoError(t, err)

	got := entriesOfKind(mem.puts, pttag.KindName)
	require.Len(t, got, 1)
	var rec pttag.TagRecord
	require.NoError(t, json.Unmarshal(got[0].Content, &rec))
	assert.Equal(t, []string{"doc:d1", "doc:d2"}, rec.Sources)
}

// TestADerivedTagNamesNoSources: its provenance IS its DerivedFrom, and naming
// resources there would claim a derivation the schema does not make.
func TestADerivedTagNamesNoSources(t *testing.T) {
	subs := &fakeSubjects{}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		DerivedFrom: []string{"ptt-a", "ptt-b"},
	})
	require.NoError(t, err)

	got := entriesOfKind(mem.puts, pttag.KindName)
	require.Len(t, got, 1)
	var rec pttag.TagRecord
	require.NoError(t, json.Unmarshal(got[0].Content, &rec))
	assert.Empty(t, rec.Sources)
	assert.Equal(t, []string{"ptt-a", "ptt-b"}, rec.DerivedFrom)
}
