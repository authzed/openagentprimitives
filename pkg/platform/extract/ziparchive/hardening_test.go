package ziparchive_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	"github.com/authzed/openagentprimitives/pkg/platform/extract/ziparchive"
)

// The deadline was enforced only inside a member's Read, so a walk made
// entirely of SKIPPED entries — traversal names, non-regular entries,
// collisions — ran with no time bound at all. Cheap per entry, but a bound
// that does not cover the loop it is meant to bound is not a bound.
func TestWallClockCoversTheSkipPath(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 2000; i++ {
		w, err := zw.Create("../escape/f.txt")
		require.NoError(t, err)
		_, err = w.Write([]byte("x"))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	lim := extract.DefaultLimits
	lim.MaxWallClock = time.Nanosecond // already expired when the walk starts

	_, err := ziparchive.Backend{}.Explode(bytes.NewReader(buf.Bytes()), int64(buf.Len()), lim,
		func(extract.Member) error { return nil })
	require.ErrorIs(t, err, extract.ErrArchiveBomb,
		"an expired deadline must stop the walk even when every entry is skipped")
}

// Open closed over ONE shared boundedReader, so a second call overwrote the
// first reader without closing it (a leak) and, worse, inherited the first
// read's byte count — so a re-read tripped the MEMBER bound on bytes it had
// already been allowed once.
//
// MaxMemberBytes sits between one read and two on purpose. With a generous
// bound the shared reader still returns correct bytes and the defect is
// invisible, which is how the first version of this test passed against the
// bug it was written for.
func TestOpenIsSafeToCallTwice(t *testing.T) {
	const memberSize = 4096
	body := bytes.Repeat([]byte("A"), memberSize)
	archive := buildSimpleZip(t, map[string][]byte{"a.log": body})

	lim := generousLimits
	lim.MaxMemberBytes = memberSize + 1024 // one read fits; two would not, if they accumulated

	var first, second []byte
	_, err := ziparchive.Backend{}.Explode(bytes.NewReader(archive), int64(len(archive)), lim,
		func(m extract.Member) error {
			rc1, oerr := m.Open()
			require.NoError(t, oerr)
			first, _ = io.ReadAll(rc1)
			require.NoError(t, rc1.Close())

			rc2, oerr := m.Open()
			require.NoError(t, oerr)
			var rerr error
			second, rerr = io.ReadAll(rc2)
			require.NoError(t, rc2.Close())
			return rerr
		})
	require.NoError(t, err, "a second Open must not trip the member bound on bytes already allowed once")

	assert.Equal(t, body, first)
	assert.Equal(t, body, second,
		"a second Open must read the member from the start with a fresh member counter")
}

// Re-reading must not be a way around the ARCHIVE total: each Open gets a
// fresh member counter, but every byte still counts once toward the archive.
func TestRepeatedOpenStillCountsTowardTheArchiveTotal(t *testing.T) {
	body := bytes.Repeat([]byte("A"), 8192)
	archive := buildSimpleZip(t, map[string][]byte{"a.log": body})

	lim := generousLimits
	lim.MaxUncompressedTotal = 20000 // ~2.4 reads' worth

	var reads int
	_, err := ziparchive.Backend{}.Explode(bytes.NewReader(archive), int64(len(archive)), lim,
		func(m extract.Member) error {
			for i := 0; i < 5; i++ {
				rc, oerr := m.Open()
				if oerr != nil {
					return oerr
				}
				_, cerr := io.Copy(io.Discard, rc)
				_ = rc.Close()
				reads++
				if cerr != nil {
					return cerr
				}
			}
			return nil
		})
	require.ErrorIs(t, err, extract.ErrArchiveBomb,
		"repeated Opens must accumulate against the archive total, not reset it")
	assert.Less(t, reads, 5, "the walk must stop once the archive total is exceeded")
}

// The declared-total pre-filter summed into a uint64 that an attacker can wrap.
func TestDeclaredTotalOverflowDoesNotDefeatThePreFilter(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 4; i++ {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name: fmt.Sprintf("f%d.bin", i), Method: zip.Store,
		})
		require.NoError(t, err)
		_, err = w.Write([]byte("x"))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	// Not directly constructible through archive/zip's writer, so this asserts
	// the ARITHMETIC rather than a crafted file: a saturating accumulator can
	// never wrap, so no combination of declared sizes slips under the ceiling.
	assert.True(t, ziparchive.DeclaredTotalExceeds(
		[]uint64{^uint64(0), ^uint64(0), 1}, 64<<20),
		"a wrapping sum would report false here and let an over-declared archive through")
	assert.False(t, ziparchive.DeclaredTotalExceeds([]uint64{1, 2, 3}, 64<<20))
}
