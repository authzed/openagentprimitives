package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractHandler covers the status-code contract POST /extract must
// hold: a registered MIME extracts, an unregistered one is 415 (not a
// 5xx — the caller uses that distinction to pick a permanent vs. transient
// notice), a malformed body for a registered MIME is 422, and a non-POST
// method is 405. All four share one server and differ only in
// request/expectation, hence the table.
func TestExtractHandler(t *testing.T) {
	srv := httptest.NewServer(routes(defaultMaxBytes))
	defer srv.Close()

	cases := []struct {
		name       string
		method     string
		mime       string
		body       string
		wantStatus int
		wantText   string // checked only when non-empty
	}{
		{
			name:       "registered MIME (text/plain): 200 with extracted text",
			method:     http.MethodPost,
			mime:       "text/plain",
			body:       "hello world",
			wantStatus: http.StatusOK,
			wantText:   "hello world",
		},
		{
			name:       "unregistered MIME: 415, not a 5xx",
			method:     http.MethodPost,
			mime:       "application/x-bogus-format",
			body:       "irrelevant",
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:       "malformed body for a registered MIME (application/pdf): 422",
			method:     http.MethodPost,
			mime:       "application/pdf",
			body:       "this is not a real pdf, just garbage bytes",
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name:       "GET /extract: 405 method not allowed",
			method:     http.MethodGet,
			mime:       "text/plain",
			body:       "",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, srv.URL+"/extract", strings.NewReader(tc.body))
			require.NoError(t, err, "build request")
			req.Header.Set("Content-Type", tc.mime)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "do request")
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			if tc.wantText != "" {
				var payload extractResponse
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload), "decode extract response")
				assert.Equal(t, tc.wantText, payload.Text)
			}
		})
	}
}

// TestExtractHandler_BodyOverCapReturns413 needs its own server: it's the
// one case where the CONFIGURED cap itself (not a fixed-size fixture) is
// under test, so it runs against a deliberately tiny maxBytes rather than
// the shared table's default.
func TestExtractHandler_BodyOverCapReturns413(t *testing.T) {
	const tinyCap = 8 // bytes
	srv := httptest.NewServer(routes(tinyCap))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/extract", strings.NewReader("this body is longer than eight bytes"))
	require.NoError(t, err, "build request")
	req.Header.Set("Content-Type", "text/plain")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "do request")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

// TestExtractHandler_MalformedInput_DoesNotLogFileContent is a regression
// test: pkg/platform/extract/tabula wraps upstream parser errors
// (github.com/tsawler/tabula) that can embed raw bytes read from the
// uploaded file directly in the error string — e.g. a malformed PDF
// produces "...invalid PDF header: <first bytes of the body>". This pod's
// whole rationale is that uploaded bytes never leave it except as extracted
// text in the HTTP response, so neither the log stream nor the response
// body may ever contain them. It replaces slog's default handler with one
// writing to a buffer this test can inspect, then restores it.
func TestExtractHandler_MalformedInput_DoesNotLogFileContent(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	srv := httptest.NewServer(routes(defaultMaxBytes))
	defer srv.Close()

	const secretBody = "this is not a real pdf, just garbage bytes"
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/extract", strings.NewReader(secretBody))
	require.NoError(t, err, "build request")
	req.Header.Set("Content-Type", "application/pdf")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "do request")
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")

	logged := logBuf.String()
	t.Logf("captured log line: %s", strings.TrimSpace(logged))
	assert.NotContains(t, logged, secretBody, "log stream must not contain raw uploaded bytes")
	assert.NotContains(t, logged, "this is", "log stream must not contain any prefix of the raw uploaded bytes")
	assert.NotContains(t, string(respBody), secretBody, "HTTP response must not echo raw uploaded bytes")
	// The classification must still be present — content-free, but not silent.
	assert.Contains(t, logged, "errKind=parse_failed", "log line must still carry a content-free classification")
	assert.Contains(t, logged, "mime=application/pdf")
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(routes(defaultMaxBytes))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err, "GET /healthz")
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
