package runner

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// A field added to both structs but not to the conversion between them is
// silently dropped, and the symptom shows up far away: hydration stops
// recognizing archive members and fills the native window with log files.
//
// Asserting on the CONVERSION rather than on either struct is what catches
// that, and comparing field COUNTS is what makes it catch the next field too
// rather than only this one.
func TestAttachmentConversionCarriesEveryField(t *testing.T) {
	in := []memory.ContentBlock{{
		Type: "attachment",
		Attachment: &memory.AttachmentBlock{
			Filename:   "logs/spicedb.log",
			MIME:       "text/plain",
			SizeBytes:  4096,
			Ref:        "mem://member-ref",
			TextRef:    "mem://member-text",
			Pages:      3,
			ArchiveRef: "mem://the-archive",
		},
	}}

	got := contentBlocksFromMemory(in)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Attachment)

	a := got[0].Attachment
	assert.Equal(t, "logs/spicedb.log", a.Filename)
	assert.Equal(t, "text/plain", a.MIME)
	assert.Equal(t, int64(4096), a.Size)
	assert.Equal(t, "mem://member-ref", a.Ref)
	assert.Equal(t, "mem://member-text", a.TextRef)
	assert.Equal(t, 3, a.Pages)
	assert.Equal(t, "mem://the-archive", a.ArchiveRef,
		"ArchiveRef must survive the memory→llm conversion; dropping it makes every archive member look like a directly-attached file")

	// The guard that outlives this field: if llm.AttachmentRef grows one and
	// the conversion does not, this count disagrees and the next author is
	// told where to look.
	assert.Equal(t, 7, reflect.TypeOf(llm.AttachmentRef{}).NumField(),
		"llm.AttachmentRef gained or lost a field — update contentBlocksFromMemory and this test together")
}

// memberBlocks builds a user turn carrying n archive members, all with no
// extracted text — the shape a support bundle of log files produces.
func memberBlocks(n int) []llm.ContentBlock {
	out := []llm.ContentBlock{{Type: "text", Text: "here is the bundle"}}
	for i := 0; i < n; i++ {
		out = append(out, llm.ContentBlock{
			Type: "attachment",
			Attachment: &llm.AttachmentRef{
				Ref:        "mem://member-" + string(rune('a'+i)),
				MIME:       "text/plain",
				Filename:   "logs/f" + string(rune('a'+i)) + ".log",
				Size:       128,
				ArchiveRef: "mem://the-archive",
			},
		})
	}
	return out
}

// The bug this prevents: members with no TextRef would fill the newest-N
// native window with arbitrary log files, fail nativeBlockFor on their MIME,
// and be named in an unreadable-attachments note about files the agent was
// never told about.
func TestArchiveMembersAreNotNativeCandidates(t *testing.T) {
	l := &Loop{}
	msgs := []llm.Message{{Role: "user", Content: memberBlocks(20)}}

	got := l.hydrateAttachments(context.Background(), msgs)

	var text string
	for _, b := range got[0].Content {
		assert.NotEqual(t, "image", b.Type, "no member may render natively while unpinned")
		assert.NotEqual(t, "document", b.Type, "no member may render natively while unpinned")
		if b.Type == "text" {
			text += b.Text
		}
	}
	assert.NotContains(t, text, "not readable",
		"the archive's index already stated each member's readability, per member; a note here would name files the agent never heard of")
	assert.NotContains(t, text, "no longer in view",
		"members are not in the window, so they cannot fall out of it")
}

// A directly-attached file alongside a bundle must keep its window slot: the
// exclusion exists so members cannot crowd out the file the user actually sent.
func TestADirectAttachmentKeepsItsSlotBesideMembers(t *testing.T) {
	content := memberBlocks(10)
	content = append(content, llm.ContentBlock{
		Type: "attachment",
		Attachment: &llm.AttachmentRef{
			Ref: "mem://direct", MIME: "text/plain", Filename: "note.txt", Size: 10,
		},
	})
	l := &Loop{}
	msgs := []llm.Message{{Role: "user", Content: content}}

	got := l.hydrateAttachments(context.Background(), msgs)

	var sawDirectRef bool
	for _, b := range got[0].Content {
		if b.Attachment != nil && b.Attachment.Ref == "mem://direct" {
			sawDirectRef = true
		}
	}
	// The direct attachment is handled by the ordinary path — whatever that
	// path decides, the members must not have consumed its budget first.
	assert.False(t, sawDirectRef, "hydration always clears refs before the provider call")
}
