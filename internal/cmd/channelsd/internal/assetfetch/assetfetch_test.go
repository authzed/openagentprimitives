package assetfetch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/internal/cmd/channelsd/internal/assetfetch"
)

func TestFetch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TOK" {
			http.Error(w, "no", 401)
			return
		}
		if r.URL.Path != "/artifact-bundle/default/sess1/ar-1/bundle" {
			http.Error(w, "wrong path", 404)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Disposition", `attachment; filename="report.html"`)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()
	f := assetfetch.New(assetfetch.Config{OperatorURL: srv.URL, Token: "TOK"})
	out, err := f.Fetch(context.Background(), "default", "sess1", "ar-1")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(out.Bytes))
	assert.Equal(t, "text/html", out.MIME)
	assert.Equal(t, "report.html", out.Filename)
}

// TestFetch_ZipPassthrough proves Fetch has no bundle-vs-raw branching of
// its own: it hits the one bundle route and passes through whatever comes
// back verbatim, including a zipped response (the server-side decision made
// in pkg/memory/httpsrv's serveBundle for an html primary with resolvable
// artifact: refs).
func TestFetch_ZipPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/artifact-bundle/default/sess1/ar-1/bundle" {
			http.Error(w, "wrong path", 404)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="report.zip"`)
		_, _ = w.Write([]byte("PK\x03\x04fake-zip-bytes"))
	}))
	defer srv.Close()
	f := assetfetch.New(assetfetch.Config{OperatorURL: srv.URL, Token: "TOK"})
	out, err := f.Fetch(context.Background(), "default", "sess1", "ar-1")
	require.NoError(t, err)
	assert.Equal(t, "PK\x03\x04fake-zip-bytes", string(out.Bytes))
	assert.Equal(t, "application/zip", out.MIME)
	assert.Equal(t, "report.zip", out.Filename)
}

func TestFetch_PropagatesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", 403)
	}))
	defer srv.Close()
	f := assetfetch.New(assetfetch.Config{OperatorURL: srv.URL, Token: "x"})
	_, err := f.Fetch(context.Background(), "default", "sess1", "ar-1")
	require.Error(t, err, "expected error on 403")
}
