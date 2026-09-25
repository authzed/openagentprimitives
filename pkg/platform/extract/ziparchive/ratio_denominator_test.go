package ziparchive

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The ratio bound divides the bytes produced by the member's DECLARED
// compressed size, and archive/zip does not check that declaration against the
// bytes actually present. So an archive could declare a huge compressed size
// for a tiny member, drive the denominator up, and make the ratio check never
// trip — a bound switched off by writing a number.
//
// A member's compressed bytes cannot exceed the size of the archive containing
// them. Clamping the denominator to that is a fact about the container rather
// than a claim by its author, and it costs one comparison.
//
// It does not make the declaration trustworthy: an UNDER-declaration still
// tightens the ratio (which is safe — it only refuses sooner), and the absolute
// per-member and total bounds remain the real defense. What it removes is the
// ability to disable the ratio check entirely from inside the file.

func TestUsableCompressedSize_ClampsAnOverDeclarationToTheArchiveSize(t *testing.T) {
	// A 4 KB archive whose member claims to be a gigabyte compressed.
	got := usableCompressedSize(1<<30, 4096)

	assert.Equal(t, int64(4096), got,
		"a member cannot hold more compressed bytes than the archive it lives in")
}

func TestUsableCompressedSize_KeepsAnHonestDeclaration(t *testing.T) {
	got := usableCompressedSize(900, 4096)

	assert.Equal(t, int64(900), got, "a plausible declaration is used as written")
}

// The pre-existing fail-closed cases, unchanged: a zero or wrapping declaration
// yields the strictest denominator so the ratio bound trips at MaxRatio bytes
// rather than being skipped.
func TestUsableCompressedSize_UnusableDeclarationsStayStrict(t *testing.T) {
	assert.Equal(t, int64(1), usableCompressedSize(0, 4096), "zero is not a denominator")
	assert.Equal(t, int64(1), usableCompressedSize(math.MaxUint64, 4096), "a wrapping value is not a denominator")
}

// An unknown archive size (a caller that could not report one) must not clamp
// to zero, which would make every denominator 1 and refuse every archive.
func TestUsableCompressedSize_UnknownArchiveSizeDoesNotClamp(t *testing.T) {
	assert.Equal(t, int64(900), usableCompressedSize(900, 0))
}
