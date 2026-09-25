package ziparchive_test

import (
	"archive/zip"
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	"github.com/authzed/openagentprimitives/pkg/platform/extract/ziparchive"
)

// Fixtures are BUILT here rather than committed as binaries. A committed zip
// bomb is a landmine in the repo, and a reviewer cannot read one to check that
// it tests what its filename claims.

// buildZip writes members into an in-memory zip, in a stable order so a test
// that asserts on member ORDER is reproducible.
func buildZip(t *testing.T, names []string, bodies map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		require.NoError(t, err)
		_, err = w.Write(bodies[n])
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// buildSimpleZip is buildZip for the common case where order does not matter.
func buildSimpleZip(t *testing.T, bodies map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(bodies))
	for n := range bodies {
		names = append(names, n)
	}
	return buildZip(t, names, bodies)
}

// buildRatioBomb writes one member of highly-compressible bytes, so the
// archive is tiny and its expansion is not.
func buildRatioBomb(t *testing.T, name string, uncompressed int) []byte {
	t.Helper()
	return buildSimpleZip(t, map[string][]byte{name: bytes.Repeat([]byte{'A'}, uncompressed)})
}

// explodeAll drains every member, which is what makes a streaming bound
// observable: a limit enforced only on Open() and not on Read() would pass a
// test that never read a body.
func explodeAll(t *testing.T, archive []byte, lim extract.Limits) ([]extract.Member, extract.Summary, error) {
	t.Helper()
	var got []extract.Member
	sum, err := ziparchive.Backend{}.Explode(
		bytes.NewReader(archive), int64(len(archive)), lim,
		func(m extract.Member) error {
			rc, oerr := m.Open()
			if oerr != nil {
				return oerr
			}
			defer func() { _ = rc.Close() }()
			if _, cerr := io.Copy(io.Discard, rc); cerr != nil {
				return cerr
			}
			got = append(got, m)
			return nil
		})
	return got, sum, err
}

// generousLimits is for tests whose subject is not a bound.
var generousLimits = extract.Limits{
	MaxUncompressedTotal: 64 << 20,
	MaxMemberBytes:       25 << 20,
	MaxMembers:           256,
	MaxRatio:             1 << 20,
	MaxWallClock:         time.Minute,
}
