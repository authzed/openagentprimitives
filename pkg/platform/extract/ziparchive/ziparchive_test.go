package ziparchive_test

import (
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

func TestExplodeRefusesBombs(t *testing.T) {
	cases := []struct {
		name    string
		archive func(t *testing.T) []byte
		lim     extract.Limits
		wantErr error
	}{
		{
			name:    "single ratio bomb: one tiny member expanding enormously",
			archive: func(t *testing.T) []byte { return buildRatioBomb(t, "bomb", 32<<20) },
			lim: extract.Limits{
				MaxUncompressedTotal: 1 << 20, MaxMemberBytes: 1 << 20,
				MaxMembers: 10, MaxRatio: 100, MaxWallClock: time.Minute,
			},
			wantErr: extract.ErrArchiveBomb,
		},
		{
			name: "distributed bomb: many modest members over the archive total",
			archive: func(t *testing.T) []byte {
				bodies := map[string][]byte{}
				for i := 0; i < 16; i++ {
					bodies[fmt.Sprintf("f%02d", i)] = bytes.Repeat([]byte{'B'}, 1<<20)
				}
				return buildSimpleZip(t, bodies)
			},
			lim: extract.Limits{
				MaxUncompressedTotal: 4 << 20, MaxMemberBytes: 25 << 20,
				MaxMembers: 256, MaxRatio: 1 << 20, MaxWallClock: time.Minute,
			},
			wantErr: extract.ErrArchiveBomb,
		},
		{
			name:    "malformed: truncated archive",
			archive: func(t *testing.T) []byte { return buildSimpleZip(t, map[string][]byte{"a": []byte("x")})[:12] },
			lim:     generousLimits,
			wantErr: extract.ErrArchiveMalformed,
		},
		{
			name:    "malformed: not a zip at all",
			archive: func(t *testing.T) []byte { return []byte("this is plainly not a zip archive") },
			lim:     generousLimits,
			wantErr: extract.ErrArchiveMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := explodeAll(t, tc.archive(t), tc.lim)
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// A member-COUNT overrun truncates rather than refusing: a partial support
// bundle is still useful. A BYTE or RATIO overrun does not, because we stopped
// mid-expansion and cannot characterize what is left — reporting that as a
// partial success would invite reasoning from a fragment believed to be whole.
func TestMemberCountOverrunTruncatesRatherThanFailing(t *testing.T) {
	bodies := map[string][]byte{}
	for i := 0; i < 40; i++ {
		bodies[fmt.Sprintf("f%02d", i)] = []byte("x")
	}
	lim := generousLimits
	lim.MaxMembers = 8

	got, sum, err := explodeAll(t, buildSimpleZip(t, bodies), lim)
	require.NoError(t, err, "a count overrun is not a refusal")
	assert.Len(t, got, 8)
	assert.True(t, sum.Truncated)
	assert.Contains(t, sum.TruncatedReason, "member count")
	assert.Equal(t, 8, sum.Members)
}

// The property that separates a real guard from one that only looks right. A
// check that fires after full expansion passes every error assertion above
// while OOMing in production, so this asserts on bytes ACTUALLY PRODUCED.
func TestBoundsTripDuringStreamingNotAfter(t *testing.T) {
	const uncompressed = 32 << 20
	archive := buildRatioBomb(t, "bomb", uncompressed)
	// MaxUncompressedTotal is set HIGH deliberately, so the declared-total
	// pre-filter does NOT fire and the member bound is what stops the copy.
	// With a low total the pre-filter catches this archive before a byte is
	// produced, `produced` is 0, and this test passes while asserting nothing
	// about streaming — the exact way a guard test goes quietly hollow.
	lim := extract.Limits{
		MaxUncompressedTotal: 64 << 20, MaxMemberBytes: 1 << 20,
		MaxMembers: 10, MaxRatio: 1 << 30, MaxWallClock: time.Minute,
	}

	var produced int64
	_, err := ziparchive.Backend{}.Explode(
		bytes.NewReader(archive), int64(len(archive)), lim,
		func(m extract.Member) error {
			rc, oerr := m.Open()
			require.NoError(t, oerr)
			defer func() { _ = rc.Close() }()
			n, _ := io.Copy(io.Discard, rc)
			produced += n
			return nil
		})
	require.ErrorIs(t, err, extract.ErrArchiveBomb)
	assert.Less(t, produced, int64(4<<20),
		"the bound must stop the copy mid-member; %d bytes were produced against a 1 MiB ceiling", produced)
}

// Depth is 0 by construction: nothing in the backend calls Explode again.
func TestNestedArchiveIsStoredNeverOpened(t *testing.T) {
	inner := buildSimpleZip(t, map[string][]byte{"deep.txt": []byte("the inner secret")})
	outer := buildSimpleZip(t, map[string][]byte{"inner.zip": inner})

	got, _, err := explodeAll(t, outer, generousLimits)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "inner.zip", got[0].Name)
	assert.Equal(t, "application/zip", got[0].MIME, "sniffed, and reported as an archive")
	for _, m := range got {
		assert.NotEqual(t, "deep.txt", m.Name, "recursion is forbidden")
	}
}

func TestHostileNamesAreSkippedNotRewritten(t *testing.T) {
	for _, name := range []string{
		"../escape.txt",
		"a/../../escape.txt",
		"/abs/escape.txt",
	} {
		t.Run(name, func(t *testing.T) {
			got, sum, err := explodeAll(t, buildSimpleZip(t, map[string][]byte{name: []byte("x")}), generousLimits)
			require.NoError(t, err, "one hostile name skips its member, it does not fail the archive")
			assert.Empty(t, got, "the member must be skipped, never rewritten into a safe name")
			assert.Equal(t, 1, sum.Skipped[extract.SkipTraversalName])
		})
	}
}

func TestNonRegularEntriesAreSkippedAndCounted(t *testing.T) {
	// A directory entry is the portable non-regular case archive/zip will
	// round-trip: a trailing slash makes it a directory.
	got, sum, err := explodeAll(t, buildSimpleZip(t, map[string][]byte{
		"logs/":      {},
		"logs/a.log": []byte("hello"),
	}), generousLimits)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the regular file is yielded")
	assert.Equal(t, "logs/a.log", got[0].Name)
	assert.Equal(t, 1, sum.Skipped[extract.SkipNonRegular], "the skip is counted, never silent")
}

// The test that proves the extension is not trusted. Trusting it would let a
// hostile filename decide which parser runs downstream.
func TestMIMEComesFromBytesNotExtension(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 600)...)

	got, _, err := explodeAll(t, buildSimpleZip(t, map[string][]byte{"totally-a.txt": png}), generousLimits)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "image/png", got[0].MIME,
		"the extension is attacker-controlled; sniffing is what makes it inert")
}

func TestTextMemberSniffsAsText(t *testing.T) {
	got, _, err := explodeAll(t, buildSimpleZip(t, map[string][]byte{
		"spicedb.log": []byte("2026-08-27T10:14:02Z WARN dispatch queue depth 3\n"),
	}), generousLimits)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].MIME, "text/plain")
	assert.Positive(t, got[0].Size)
}

func TestRegisteredForZipMIMEs(t *testing.T) {
	for _, m := range []string{"application/zip", "application/x-zip-compressed"} {
		_, ok := extract.ExploderFor(m)
		assert.True(t, ok, "%s must resolve to the zip backend", m)
	}
	// And it must NOT have claimed an extractor slot.
	_, isExtractor := extract.For("application/zip")
	assert.False(t, isExtractor)
}

// The pre-filter's own path: an archive whose DECLARED total is over the
// ceiling is refused before a single byte is produced.
//
// Held apart from TestBoundsTripDuringStreamingNotAfter because the two guard
// different attackers — an honest bomb, and one that lies about its sizes.
// Only the streaming bound can catch the liar, so neither test substitutes for
// the other.
func TestDeclaredTotalIsRefusedBeforeStreaming(t *testing.T) {
	archive := buildRatioBomb(t, "bomb", 32<<20)
	lim := extract.Limits{
		MaxUncompressedTotal: 1 << 20, MaxMemberBytes: 25 << 20,
		MaxMembers: 10, MaxRatio: 1 << 30, MaxWallClock: time.Minute,
	}

	var yielded int
	_, err := ziparchive.Backend{}.Explode(
		bytes.NewReader(archive), int64(len(archive)), lim,
		func(extract.Member) error { yielded++; return nil })

	require.ErrorIs(t, err, extract.ErrArchiveBomb)
	assert.Zero(t, yielded, "an over-declared archive must be refused before any member is handed over")
}

// The registry answers "can this process open one?"; IsContainerMIME answers
// "is this a container at all?", and channelsd relies on the second to pick a
// time budget without linking a zip parser. If a backend claimed a MIME the
// container vocabulary did not know, that upload would silently get the
// single-file budget and time out on a large bundle.
func TestClaimedMIMEsAreContainerMIMEs(t *testing.T) {
	for _, m := range (ziparchive.Backend{}).MIMEs() {
		assert.True(t, extract.IsContainerMIME(m),
			"%s is claimed by the zip exploder but missing from the container vocabulary", m)
	}
}
