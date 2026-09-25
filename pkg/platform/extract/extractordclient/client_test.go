package extractordclient_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/platform/extract/extractordclient"
)

func TestExtract_200DecodesTextAndPages(t *testing.T) {
	var gotMIME, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMIME = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"extracted document text","pages":4}`))
	}))
	t.Cleanup(srv.Close)

	c := extractordclient.New(srv.URL)
	text, pages, err := c.Extract(context.Background(), "application/pdf", strings.NewReader("raw pdf bytes"))
	require.NoError(t, err)
	assert.Equal(t, "extracted document text", text)
	assert.Equal(t, 4, pages)
	assert.Equal(t, "application/pdf", gotMIME, "mime must ride the Content-Type header, matching extractord's contract")
	assert.Equal(t, "raw pdf bytes", gotBody, "the body must be streamed through untouched")
}

func TestExtract_415MapsToErrAttachmentMIMEUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
	}))
	t.Cleanup(srv.Close)

	c := extractordclient.New(srv.URL)
	_, _, err := c.Extract(context.Background(), "application/x-nonsense", strings.NewReader("bytes"))
	require.Error(t, err)
	assert.ErrorIs(t, err, httpsrv.ErrAttachmentMIMEUnsupported,
		"415 is the ONLY permanent outcome — every other failure must stay a plain (transient) error")
}

// TestExtract_NonOKStatusIsAPlainTransientError covers every non-2xx,
// non-415 status extractord's own contract documents (internal/cmd/extractord/main.go's
// handleExtract: 413 too large, 422 unprocessable, plus a 5xx the server
// itself never emits today but the client must still handle correctly since
// extractord is a separate deployable that can be fronted by anything), and
// the malformed-response-body case a status-code table alone can't cover: a
// 200 whose body isn't valid JSON must surface as a decode error, not a
// silent zero-value {"", 0} success.
func TestExtract_NonOKStatusIsAPlainTransientError(t *testing.T) {
	statusCases := []struct {
		name   string
		status int
	}{
		{name: "413 too large", status: http.StatusRequestEntityTooLarge},
		{name: "422 unprocessable (corrupt/malformed input)", status: http.StatusUnprocessableEntity},
		{name: "500 internal server error", status: http.StatusInternalServerError},
		{name: "503 service unavailable", status: http.StatusServiceUnavailable},
	}
	for _, tc := range statusCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "could not extract text from this file", tc.status)
			}))
			t.Cleanup(srv.Close)

			c := extractordclient.New(srv.URL)
			_, _, err := c.Extract(context.Background(), "application/pdf", strings.NewReader("bytes"))
			require.Error(t, err)
			assert.NotErrorIs(t, err, httpsrv.ErrAttachmentMIMEUnsupported,
				"only 415 is the permanent unsupported-MIME outcome — every other status must stay a plain (transient) error")
		})
	}

	t.Run("200 with malformed JSON body -> decode error, not a silent empty success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{not valid json`))
		}))
		t.Cleanup(srv.Close)

		c := extractordclient.New(srv.URL)
		text, pages, err := c.Extract(context.Background(), "application/pdf", strings.NewReader("bytes"))
		require.Error(t, err)
		assert.NotErrorIs(t, err, httpsrv.ErrAttachmentMIMEUnsupported)
		assert.Empty(t, text)
		assert.Zero(t, pages)
	})
}

func TestExtract_ConnectionFailureIsATransientError(t *testing.T) {
	// A closed server: connections to this URL fail outright, exercising the
	// transport-error path (never reaches a status code at all).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := extractordclient.New(srv.URL)
	_, _, err := c.Extract(context.Background(), "application/pdf", strings.NewReader("bytes"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, httpsrv.ErrAttachmentMIMEUnsupported)
}
