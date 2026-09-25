package icons

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discoverer returns a Discoverer wired to an unconstrained http.Client
// (the production wiring uses pkg/x/safehttp.Client; tests use a plain
// client to talk to httptest servers).
func discoverer(t *testing.T) *Discoverer {
	t.Helper()
	return &Discoverer{
		HTTPClient:   &http.Client{Timeout: 2 * time.Second},
		MaxBodyBytes: 1 << 20, // 1 MiB cap for HTML
		MaxIconBytes: 256 << 10,
	}
}

func TestDiscover_PNGFromLinkRel(t *testing.T) {
	iconBytes := []byte("\x89PNG\r\n\x1a\nfake-png-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head>
                <link rel="icon" type="image/png" sizes="64x64" href="/static/favicon-64.png">
            </head></html>`)
		case "/static/favicon-64.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(iconBytes)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	e, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "image/png", e.ContentType)
	assert.Equal(t, iconBytes, e.Bytes)
}

func TestDiscover_RefusesSVGAndTakesThePNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head>
                <link rel="icon" type="image/png" sizes="64x64" href="/png-icon.png">
                <link rel="icon" type="image/svg+xml" href="/svg-icon.svg">
            </head></html>`)
		case "/svg-icon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
		case "/png-icon.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = fmt.Fprint(w, "\x89PNG\r\n\x1a\n")
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	e, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "image/png", e.ContentType,
		"SVG used to beat PNG deterministically; these bytes are served on the session origin, and SVG is the one image format that is also a script host")
	assert.NotContains(t, string(e.Bytes), "<script")
}

func TestDiscover_PrefersLargerSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head>
                <link rel="icon" type="image/png" sizes="16x16" href="/small.png">
                <link rel="icon" type="image/png" sizes="128x128" href="/large.png">
            </head></html>`)
		case "/large.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("large-png"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	e, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, []byte("large-png"), e.Bytes, "larger sizes attribute must win within same format")
}

func TestDiscover_FaviconIcoFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			// HTML has no <link rel="icon"> tags
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head><title>x</title></head></html>`)
		case "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte("ico-bytes"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	e, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "image/x-icon", e.ContentType)
	assert.Equal(t, []byte("ico-bytes"), e.Bytes)
}

func TestDiscover_RelativeHrefResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head>
                <link rel="icon" type="image/png" href="favicon-relative.png">
            </head></html>`)
		case "/favicon-relative.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("relative-png"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	e, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, []byte("relative-png"), e.Bytes)
}

func TestDiscover_RejectsTooLargeIcon(t *testing.T) {
	huge := make([]byte, 512<<10) // 512 KiB > 256 KiB cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<html><head><link rel="icon" type="image/png" href="/big.png"></head></html>`)
		case "/big.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(huge)))
			_, _ = w.Write(huge)
		case "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write([]byte("ico-fallback"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	e, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "image/x-icon", e.ContentType, "oversized PNG must fall through to /favicon.ico")
	assert.Equal(t, []byte("ico-fallback"), e.Bytes)
}

func TestDiscover_NotFoundEverywhere_ReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	_, err := discoverer(t).Discover(context.Background(), srv.URL)
	require.Error(t, err, "all paths 404 → caller falls back to SVG")
}
