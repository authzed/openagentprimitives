package extractordclient_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/platform/extract"
	"github.com/authzed/openagentprimitives/pkg/platform/extract/extractordclient"
)

// serveMembers writes a well-formed multipart explode response.
func serveMembers(t *testing.T, w http.ResponseWriter, names []string, bodies map[string]string, summary string) {
	t.Helper()
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	for _, n := range names {
		pw, err := mw.CreatePart(map[string][]string{
			"Content-Type":  {"text/plain"},
			"X-Member-Name": {n},
			"X-Member-Size": {"5"},
		})
		require.NoError(t, err)
		_, err = io.WriteString(pw, bodies[n])
		require.NoError(t, err)
	}
	pw, err := mw.CreatePart(map[string][]string{
		"Content-Type":  {"application/json"},
		"X-Member-Kind": {"summary"},
	})
	require.NoError(t, err)
	_, err = io.WriteString(pw, summary)
	require.NoError(t, err)
	require.NoError(t, mw.Close())
}

// The split the notice wording depends on. A 5xx re-worded as "unsupported"
// tells the user a permanent falsehood about a file that would work next time.
func TestExplodeMapsStatusToPermanentOrTransient(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantSentinel error
		transient    bool
	}{
		{"415 is permanent and its own sentinel", http.StatusUnsupportedMediaType, httpsrv.ErrArchiveMIMEUnsupported, false},
		{"413 is permanent: refused", http.StatusRequestEntityTooLarge, httpsrv.ErrArchiveRefused, false},
		{"422 is permanent: refused", http.StatusUnprocessableEntity, httpsrv.ErrArchiveRefused, false},
		{"500 is transient: neither sentinel", http.StatusInternalServerError, nil, true},
		{"503 is transient: neither sentinel", http.StatusServiceUnavailable, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)

			_, err := extractordclient.New(srv.URL).Explode(
				context.Background(), "application/zip", strings.NewReader("x"), extract.DefaultLimits,
				func(httpsrv.ExplodedMember) error { return nil })
			require.Error(t, err)

			if tc.transient {
				assert.False(t, errors.Is(err, httpsrv.ErrArchiveMIMEUnsupported),
					"a transient failure must never read as an unsupported type")
				assert.False(t, errors.Is(err, httpsrv.ErrArchiveRefused),
					"a transient failure must never read as a refusal")
				return
			}
			assert.ErrorIs(t, err, tc.wantSentinel)
		})
	}
}

// A transport failure is transient too, and must be as distinguishable as a
// 5xx — the connection dying is not a statement about the file.
func TestExplodeTransportFailureIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refuse the connection

	_, err := extractordclient.New(srv.URL).Explode(
		context.Background(), "application/zip", strings.NewReader("x"), extract.DefaultLimits,
		func(httpsrv.ExplodedMember) error { return nil })
	require.Error(t, err)
	assert.False(t, errors.Is(err, httpsrv.ErrArchiveMIMEUnsupported))
	assert.False(t, errors.Is(err, httpsrv.ErrArchiveRefused))
}

func TestExplodeYieldsEveryMemberAndTheSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveMembers(t, w, []string{"a.log", "b.log"},
			map[string]string{"a.log": "alpha", "b.log": "betaX"},
			`{"members":2,"truncated":true,"truncatedReason":"member count","skipped":[{"reason":"non_regular","count":3}]}`)
	}))
	t.Cleanup(srv.Close)

	got := map[string]string{}
	sum, err := extractordclient.New(srv.URL).Explode(
		context.Background(), "application/zip", strings.NewReader("x"), extract.DefaultLimits,
		func(m httpsrv.ExplodedMember) error {
			b, rerr := io.ReadAll(m.Body)
			if rerr != nil {
				return rerr
			}
			got[m.Name] = string(b)
			return nil
		})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"a.log": "alpha", "b.log": "betaX"}, got)
	assert.Equal(t, 2, sum.Members)
	assert.True(t, sum.Truncated, "a caller must be able to see the archive was partial")
	assert.Equal(t, "member count", sum.TruncatedReason)
	assert.Equal(t, map[string]int{"non_regular": 3}, sum.Skipped)
}

// The streaming contract: yield must run while the response is still being
// read, or the operator holds the whole expansion in memory.
func TestExplodeStreamsRatherThanBuffering(t *testing.T) {
	const members = 8
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < members; i++ {
			pw, err := mw.CreatePart(map[string][]string{
				"Content-Type":  {"text/plain"},
				"X-Member-Name": {string(rune('a'+i)) + ".log"},
				"X-Member-Size": {"1"},
			})
			require.NoError(t, err)
			_, _ = io.WriteString(pw, "x")
			if flusher != nil {
				flusher.Flush()
			}
		}
		pw, _ := mw.CreatePart(map[string][]string{"X-Member-Kind": {"summary"}})
		_, _ = io.WriteString(pw, `{"members":8}`)
		_ = mw.Close()
	}))
	t.Cleanup(srv.Close)

	var seenBeforeSummary int
	_, err := extractordclient.New(srv.URL).Explode(
		context.Background(), "application/zip", strings.NewReader("x"), extract.DefaultLimits,
		func(m httpsrv.ExplodedMember) error {
			// Reading here proves Body is a live part rather than a buffer
			// handed over after the fact.
			_, rerr := io.Copy(io.Discard, m.Body)
			seenBeforeSummary++
			return rerr
		})
	require.NoError(t, err)
	assert.Equal(t, members, seenBeforeSummary)
}

// An error from yield aborts the walk and is returned unchanged, so a caller
// whose store write failed is not told the archive was fine.
func TestExplodeReturnsYieldErrorUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveMembers(t, w, []string{"a.log", "b.log"},
			map[string]string{"a.log": "alpha", "b.log": "betaX"}, `{"members":2}`)
	}))
	t.Cleanup(srv.Close)

	sentinel := errors.New("store write failed")
	var calls int
	_, err := extractordclient.New(srv.URL).Explode(
		context.Background(), "application/zip", strings.NewReader("x"), extract.DefaultLimits,
		func(httpsrv.ExplodedMember) error { calls++; return sentinel })

	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, calls, "the walk stops at the first yield error")
}

func TestExplodeRejectsNonMultipartSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"members":0}`))
	}))
	t.Cleanup(srv.Close)

	_, err := extractordclient.New(srv.URL).Explode(
		context.Background(), "application/zip", bytes.NewReader(nil), extract.DefaultLimits,
		func(httpsrv.ExplodedMember) error { return nil })
	require.Error(t, err, "a 200 that is not multipart is a contract violation, not an empty archive")
}
