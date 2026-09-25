package pttagmint_test

// Content storage: the bytes a tag governs.
//
// pt_tag_content existed as a registered Kind with no writer and no reader for
// its whole life, so every tag labeled a datum that was nowhere — which meant a
// data slot could be bound and the child still had nothing to receive.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttag"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
)

// entriesOfKind returns the fake's Puts for one Kind.
func entriesOfKind(puts []memory.Entry, kind string) []memory.Entry {
	var out []memory.Entry
	for _, e := range puts {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestContentIsStoredUnderTheTagThatGovernsIt.
func TestContentIsStoredUnderTheTagThatGovernsIt(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{"doc:d1#viewer": {"tim"}}}
	rels, mem := &fakeRels{}, &fakeMem{}

	id, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		ToolUseID: "toolu_1",
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
		Content:   `{"diff":"one line"}`,
		MIME:      "application/json",
	})
	require.NoError(t, err)

	got := entriesOfKind(mem.puts, pttagcontent.KindName)
	require.Len(t, got, 1, "exactly one content record per mint that carries content")

	var rec pttagcontent.ContentRecord
	require.NoError(t, json.Unmarshal(got[0].Content, &rec))
	assert.Equal(t, id, rec.TagID, "the content must name the tag whose audience governs it")
	assert.Equal(t, `{"diff":"one line"}`, rec.Content)
	assert.Equal(t, "application/json", rec.MIME)
}

// TestTheTagIsWrittenBeforeItsContent.
//
// A tag with no content is a complete provenance record that simply cannot be
// handed onward. Content with no tag is bytes whose audience nothing governs.
// The order makes the first failure possible and the second impossible.
func TestTheTagIsWrittenBeforeItsContent(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{"doc:d1#viewer": {"tim"}}}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
		Content:   "bytes",
	})
	require.NoError(t, err)

	require.Len(t, mem.puts, 2)
	assert.Equal(t, pttag.KindName, mem.puts[0].Kind, "the tag first")
	assert.Equal(t, pttagcontent.KindName, mem.puts[1].Kind, "then the bytes it governs")
}

// TestAMintWithoutContentStoresNone — the norm. Every caller that only wants
// provenance passes no content, and must not have an empty record written for
// it.
func TestAMintWithoutContentStoresNone(t *testing.T) {
	subs := &fakeSubjects{byRef: map[string][]string{"doc:d1#viewer": {"tim"}}}
	rels, mem := &fakeRels{}, &fakeMem{}

	_, err := newMinter(subs, rels, mem).MintPtTag(context.Background(), testScope, memory.PtTagMintRequest{
		Resources: []memory.PtTagResourceRef{{Type: "doc", ID: "d1", Permission: "viewer"}},
	})
	require.NoError(t, err)

	assert.Empty(t, entriesOfKind(mem.puts, pttagcontent.KindName))
}
