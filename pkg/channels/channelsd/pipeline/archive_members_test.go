package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archiveAndMembers() []attachmentResult {
	return append([]attachmentResult{{
		Filename: "bundle.zip", MIME: "application/zip", SizeBytes: 4096,
		Outcome: outcomeRead, Ref: "mem://archive", TextRef: "mem://index",
	}}, memberResults("mem://archive", []InboundAssetMember{
		{Name: "logs/spicedb.log", MIME: "text/plain", SizeBytes: 900, Ref: "mem://m1", TextRef: "mem://t1"},
		{Name: "manifest.yaml", MIME: "text/plain", SizeBytes: 120, Ref: "mem://m2", TextRef: "mem://t2"},
		{Name: "shot.png", MIME: "image/png", SizeBytes: 2048, Ref: "mem://m3"},
	})...)
}

// The whole point of the index: 187 members must not become 187 lines.
func TestMembersGetNoManifestLine(t *testing.T) {
	manifest := composeAttachmentManifest(archiveAndMembers())

	assert.Contains(t, manifest, "mem://index", "the archive's index line is the ONE line")
	assert.NotContains(t, manifest, "spicedb.log")
	assert.NotContains(t, manifest, "shot.png")
	assert.Equal(t, 1, strings.Count(strings.TrimSpace(manifest), "\n")+1,
		"exactly one manifest line for the whole archive")
}

// They must not appear in the failure note either: a member is not a failure.
func TestMembersProduceNoFailureNoteOrNotice(t *testing.T) {
	items := archiveAndMembers()
	assert.Empty(t, composeAttachmentNote(items),
		"every result here either read or is a member; nothing went wrong")
	assert.Empty(t, attachmentFailureNotices(items))
}

// But they DO get blocks, which is what makes show_attachment able to pin one.
func TestMembersGetBlocksCarryingArchiveRef(t *testing.T) {
	blocks := composeAttachmentBlocks(archiveAndMembers())
	require.Len(t, blocks, 4, "the archive and all three members are recorded")

	byName := map[string]MemContent{}
	for _, b := range blocks {
		byName[b.Filename] = b
	}
	assert.Empty(t, byName["bundle.zip"].ArchiveRef, "the archive itself came from no archive")
	for _, n := range []string{"logs/spicedb.log", "manifest.yaml", "shot.png"} {
		assert.Equal(t, "mem://archive", byName[n].ArchiveRef,
			"%s must point back at its archive, or hydration cannot tell it apart from a directly-attached file", n)
	}
}

func TestOutcomeMemberIsNotAFailureClass(t *testing.T) {
	_, ok := noticeClass(outcomeMember)
	assert.False(t, ok, "a member must never raise a user-facing failure notice")
}

// An archive gets its own budget, and it is CLAMPED to the batch rather than
// added to it: the batch bound is the runner's first-turn fence.
func TestAttachmentBudget(t *testing.T) {
	cases := []struct {
		name        string
		mime        string
		remaining   time.Duration
		later       int
		wantExplode bool
		check       func(t *testing.T, got time.Duration)
	}{
		{
			name: "a non-archive always takes the single-file budget",
			mime: "application/pdf", remaining: 5 * time.Minute, later: 0, wantExplode: false,
			check: func(t *testing.T, got time.Duration) { assert.Equal(t, attachmentFetchTimeout, got) },
		},
		{
			name: "a lone archive with the whole batch left gets the archive budget",
			mime: "application/zip", remaining: 5 * time.Minute, later: 0, wantExplode: true,
			check: func(t *testing.T, got time.Duration) { assert.Equal(t, archiveFetchTimeout, got) },
		},
		{
			name: "later attachments are reserved for, so one archive cannot eat the batch",
			mime: "application/zip", remaining: 5 * time.Minute, later: 4, wantExplode: true,
			check: func(t *testing.T, got time.Duration) {
				assert.Less(t, got, archiveFetchTimeout)
				assert.GreaterOrEqual(t, got, archiveFloor)
			},
		},
		{
			name: "below the floor the archive is stored whole rather than half-exploded",
			mime: "application/zip", remaining: 20 * time.Second, later: 0, wantExplode: false,
			check: func(t *testing.T, got time.Duration) { assert.Equal(t, attachmentFetchTimeout, got) },
		},
		{
			name: "a nearly-exhausted batch never returns a negative budget",
			mime: "application/zip", remaining: time.Second, later: 9, wantExplode: false,
			check: func(t *testing.T, got time.Duration) { assert.Positive(t, got) },
		},
		{
			name: "MIME parameters do not defeat the match",
			mime: "application/zip; charset=binary", remaining: 5 * time.Minute, later: 0, wantExplode: true,
			check: func(t *testing.T, got time.Duration) { assert.Equal(t, archiveFetchTimeout, got) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, explode := attachmentBudget(tc.mime, tc.remaining, tc.later)
			assert.Equal(t, tc.wantExplode, explode)
			tc.check(t, got)
		})
	}
}

// The archive budget must never exceed what the batch has left, or the runner's
// fence expires while channelsd is still working and the agent answers blind.
func TestArchiveBudgetNeverOutlivesTheBatch(t *testing.T) {
	for _, remaining := range []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, attachmentBatchTimeout,
	} {
		got, _ := attachmentBudget("application/zip", remaining, 0)
		assert.LessOrEqual(t, got, remaining+attachmentFetchTimeout,
			"a %s remaining budget produced %s", remaining, got)
	}
}
