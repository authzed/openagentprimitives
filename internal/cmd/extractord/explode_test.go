package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract"
)

func buildZip(t *testing.T, bodies map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, b := range bodies {
		w, err := zw.Create(n)
		require.NoError(t, err)
		_, err = w.Write(b)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func buildRatioBomb(t *testing.T, uncompressed int) []byte {
	t.Helper()
	return buildZip(t, map[string][]byte{"bomb": bytes.Repeat([]byte{'A'}, uncompressed)})
}

func postExplode(t *testing.T, body []byte, mimeType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/explode", bytes.NewReader(body))
	req.Header.Set("Content-Type", mimeType)
	rec := httptest.NewRecorder()
	routes(25<<20).ServeHTTP(rec, req)
	return rec
}

type gotMember struct {
	ContentType string
	Name        string
	Size        string
	Body        []byte
}

func readMultipart(t *testing.T, rec *httptest.ResponseRecorder) (map[string]gotMember, explodeSummary) {
	t.Helper()
	_, params, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	require.NoError(t, err, "response must be multipart with a boundary")
	mr := multipart.NewReader(rec.Body, params["boundary"])

	members := map[string]gotMember{}
	var sum explodeSummary
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		b, err := io.ReadAll(p)
		require.NoError(t, err)
		if p.Header.Get(hdrMemberKind) == memberKindSummary {
			require.NoError(t, json.Unmarshal(b, &sum))
			continue
		}
		members[p.Header.Get(hdrMemberName)] = gotMember{
			ContentType: p.Header.Get("Content-Type"),
			Name:        p.Header.Get(hdrMemberName),
			Size:        p.Header.Get(hdrMemberSize),
			Body:        b,
		}
	}
	return members, sum
}

func TestExplodeReturnsMembersAndSummary(t *testing.T) {
	rec := postExplode(t, buildZip(t, map[string][]byte{
		"a.txt": []byte("alpha"),
		"b.txt": []byte("beta"),
	}), "application/zip")
	require.Equal(t, http.StatusOK, rec.Code)

	members, sum := readMultipart(t, rec)
	require.Len(t, members, 2)
	assert.Equal(t, "alpha", string(members["a.txt"].Body))
	assert.Equal(t, "beta", string(members["b.txt"].Body))
	assert.Contains(t, members["a.txt"].ContentType, "text/plain")
	assert.Equal(t, "5", members["a.txt"].Size)
	assert.Equal(t, 2, sum.Members)
	assert.False(t, sum.Truncated)
}

func TestExplodeStatusContract(t *testing.T) {
	cases := []struct {
		name string
		mime string
		body func(t *testing.T) []byte
		want int
	}{
		{
			name: "no exploder claims the MIME: 415 permanent",
			mime: "application/x-7z-compressed",
			body: func(*testing.T) []byte { return []byte("7z\xbc\xaf\x27\x1c") },
			want: http.StatusUnsupportedMediaType,
		},
		{
			name: "not an archive: 422 permanent",
			mime: "application/zip",
			body: func(*testing.T) []byte { return []byte("plainly not a zip") },
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "bomb before any member streams: 413 permanent",
			mime: "application/zip",
			body: func(t *testing.T) []byte { return buildRatioBomb(t, 128<<20) },
			want: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, postExplode(t, tc.body(t), tc.mime).Code)
		})
	}
}

// tabula's parser errors embed raw bytes read from the uploaded file, and a
// member name is file content too. Neither may reach a response body.
func TestExplodeErrorsCarryNoFileContent(t *testing.T) {
	rec := postExplode(t, buildZip(t, map[string][]byte{
		"../secret-name.txt": []byte("SENSITIVE-PAYLOAD"),
	}), "application/zip")

	assert.NotContains(t, rec.Body.String(), "secret-name")
	assert.NotContains(t, rec.Body.String(), "SENSITIVE-PAYLOAD")
}

// A skipped entry must be COUNTED in the summary and never NAMED.
func TestSummaryCountsSkipsWithoutNamingThem(t *testing.T) {
	rec := postExplode(t, buildZip(t, map[string][]byte{
		"../escape.txt": []byte("x"),
		"keep.txt":      []byte("kept"),
	}), "application/zip")
	require.Equal(t, http.StatusOK, rec.Code)

	members, sum := readMultipart(t, rec)
	require.Len(t, members, 1)
	assert.Equal(t, 1, sum.Members)
	require.Len(t, sum.Skipped, 1)
	assert.Equal(t, "traversal_name", sum.Skipped[0].Reason)
	assert.Equal(t, 1, sum.Skipped[0].Count)
	assert.NotContains(t, rec.Body.String(), "escape.txt", "a skipped name must not ride in the summary")
}

// Depth 0 travels over the wire too: the inner archive is a member, its
// contents are not.
func TestNestedArchiveArrivesAsOneMember(t *testing.T) {
	inner := buildZip(t, map[string][]byte{"deep.txt": []byte("inner")})
	rec := postExplode(t, buildZip(t, map[string][]byte{"inner.zip": inner}), "application/zip")
	require.Equal(t, http.StatusOK, rec.Code)

	members, sum := readMultipart(t, rec)
	require.Len(t, members, 1)
	assert.Contains(t, members, "inner.zip")
	assert.NotContains(t, members, "deep.txt")
	assert.Equal(t, "application/zip", members["inner.zip"].ContentType)
	assert.Equal(t, 1, sum.Members)
}

func TestExplodeRejectsNonPost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/explode", nil)
	rec := httptest.NewRecorder()
	routes(25<<20).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// The load-bearing half of per-class limits: extractord is the ONLY place an
// archive is opened, so a caller may tighten its bounds and can never loosen
// them. A limit accepted on trust would be no limit at all.
func TestLimitHeadersTightenButNeverLoosen(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    extract.Limits
	}{
		{
			name:    "no headers: the pod default",
			headers: nil,
			want:    extract.DefaultLimits,
		},
		{
			name:    "a tighter value is honored",
			headers: map[string]string{hdrLimitMembers: "8"},
			want:    withMembers(extract.DefaultLimits, 8),
		},
		{
			name:    "a LOOSER value is refused; the pod ceiling stands",
			headers: map[string]string{hdrLimitMembers: "1000000"},
			want:    extract.DefaultLimits,
		},
		{
			name:    "zero is treated as absent, never as unlimited",
			headers: map[string]string{hdrLimitMembers: "0"},
			want:    extract.DefaultLimits,
		},
		{
			name:    "negative is treated as absent",
			headers: map[string]string{hdrLimitUncompressedTotal: "-1"},
			want:    extract.DefaultLimits,
		},
		{
			name:    "garbage is treated as absent, not fatal",
			headers: map[string]string{hdrLimitRatio: "lots"},
			want:    extract.DefaultLimits,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			assert.Equal(t, tc.want, limitsFromHeaders(h))
		})
	}
}

func withMembers(l extract.Limits, n int) extract.Limits {
	l.MaxMembers = n
	return l
}

// End to end over the route: a tightened member count truncates the response
// and the summary says so, rather than the pod's larger default applying.
func TestExplodeHonorsATightenedMemberCount(t *testing.T) {
	bodies := map[string][]byte{}
	for i := 0; i < 6; i++ {
		bodies[fmt.Sprintf("f%d.txt", i)] = []byte("x")
	}
	req := httptest.NewRequest(http.MethodPost, "/explode", bytes.NewReader(buildZip(t, bodies)))
	req.Header.Set("Content-Type", "application/zip")
	req.Header.Set(hdrLimitMembers, "2")
	rec := httptest.NewRecorder()
	routes(25<<20).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	members, sum := readMultipart(t, rec)
	assert.Len(t, members, 2, "the caller's tighter bound must actually apply")
	assert.True(t, sum.Truncated)
	assert.Contains(t, sum.TruncatedReason, "member count")
}
