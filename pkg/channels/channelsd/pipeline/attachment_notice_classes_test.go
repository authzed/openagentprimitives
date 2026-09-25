package pipeline

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
)

func noticesByCategory(t *testing.T, ns []*notice.Notice) map[string]*notice.Notice {
	t.Helper()
	out := map[string]*notice.Notice{}
	for _, n := range ns {
		out[n.Category()] = n
	}
	return out
}

// The regression this task exists for. A gate-closed upload was told "For a
// temporary failure, try sending the file again" — first billing given to a
// remedy that cannot work, because one fixed NextStep hedged across every
// cause. Every positive assertion passed while that shipped, so the load-
// bearing claim here is a NEGATIVE one.
func TestNoticesNeverCarryAnotherClassesRemedy(t *testing.T) {
	got := attachmentFailureNotices([]attachmentResult{
		{Filename: "a.txt", Outcome: outcomeDisabled},
		{Filename: "b.xlsx", MIME: "application/vnd.ms-excel", Outcome: outcomeUnsupported},
		{Filename: "c.pdf", Outcome: outcomeFailed},
	})
	require.Len(t, got, 3, "three distinct causes must raise three distinct notices")

	byCat := noticesByCategory(t, got)

	notEnabled := byCat[categories.AttachmentNotEnabled]
	require.NotNil(t, notEnabled, "a closed gate must report as not-enabled")
	assert.NotContains(t, strings.ToLower(notEnabled.Args().NextStep), "again",
		"nothing was attempted; retrying cannot help")
	assert.Contains(t, strings.ToLower(notEnabled.Args().NextStep), "administrator")

	unreadable := byCat[categories.AttachmentUnreadable]
	require.NotNil(t, unreadable)
	assert.NotContains(t, strings.ToLower(unreadable.Args().NextStep), "administrator",
		"the remedy is the user's: another format")

	failed := byCat[categories.AttachmentReadFailed]
	require.NotNil(t, failed)
	assert.Contains(t, strings.ToLower(failed.Args().NextStep), "again",
		"a transient failure is the one case where resending is right")
}

// The rule the split must not break: N files of ONE class stay ONE notice.
func TestHomogeneousBatchRaisesExactlyOneNotice(t *testing.T) {
	got := attachmentFailureNotices([]attachmentResult{
		{Filename: "a.txt", Outcome: outcomeDisabled},
		{Filename: "b.txt", Outcome: outcomeDisabled},
		{Filename: "c.txt", Outcome: outcomeUnfetchable},
	})
	require.Len(t, got, 1, "three files, one cause class, one notification")
	assert.Equal(t, categories.AttachmentNotEnabled, got[0].Category())
	assert.Len(t, got[0].Args().Fields, 3, "all three files are named in it")
}

func TestReadAndStoredRaiseNoNotice(t *testing.T) {
	assert.Empty(t, attachmentFailureNotices([]attachmentResult{
		{Filename: "a.txt", Outcome: outcomeRead},
		{Filename: "b.png", Outcome: outcomeStored},
	}), "neither is a failure; outcomeStored's verdict belongs to send time")
}

func TestNoFailuresRaiseNoNotice(t *testing.T) {
	assert.Empty(t, attachmentFailureNotices(nil))
}

// Order is fixed so a mixed batch reads the same way every time, rather than
// depending on Go's map iteration.
func TestNoticeOrderIsStable(t *testing.T) {
	results := []attachmentResult{
		{Filename: "c.pdf", Outcome: outcomeFailed},
		{Filename: "b.xlsx", Outcome: outcomeUnsupported},
		{Filename: "a.txt", Outcome: outcomeDisabled},
	}
	for i := 0; i < 8; i++ {
		got := attachmentFailureNotices(results)
		require.Len(t, got, 3)
		assert.Equal(t, categories.AttachmentNotEnabled, got[0].Category())
		assert.Equal(t, categories.AttachmentUnreadable, got[1].Category())
		assert.Equal(t, categories.AttachmentReadFailed, got[2].Category())
	}
}

// Oversize and too-many are the user's to fix, like an unsupported type, so
// they ride with unreadable rather than raising a second card.
func TestOversizeAndTooManyJoinUnreadable(t *testing.T) {
	got := attachmentFailureNotices([]attachmentResult{
		{Filename: "big.bin", Outcome: outcomeOversize, SizeBytes: 100, Limit: 50},
		{Filename: "extra.txt", Outcome: outcomeTooMany, Limit: 3},
	})
	require.Len(t, got, 1)
	assert.Equal(t, categories.AttachmentUnreadable, got[0].Category())
	assert.Len(t, got[0].Args().Fields, 2)
}
