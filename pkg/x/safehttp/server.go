package safehttp

import (
	"net/http"
	"time"
)

// Server-side slow-loris defenses. These bound how long a peer may hold a
// connection open without completing a request — the failure mode where a
// handful of clients dribble bytes (or never send headers) and exhaust the
// server's connection/file-descriptor budget. They are distinct from the
// SSRF-guarded client in this package but live here as the project's single
// home for HTTP hardening.
const (
	// serverReadHeaderTimeout bounds how long a client may take to send the
	// request headers. This is THE slow-loris defense and is safe for every
	// server, including ones with long-lived streaming/SSE handlers (it only
	// times out the header phase, never the response body).
	serverReadHeaderTimeout = 10 * time.Second
	// serverIdleTimeout bounds how long an idle keep-alive connection is kept
	// open between requests.
	serverIdleTimeout = 120 * time.Second
)

// HardenServer applies streaming-safe slow-loris timeouts to s: a
// ReadHeaderTimeout and an IdleTimeout. It deliberately leaves ReadTimeout
// and WriteTimeout untouched (0) so long-lived streaming/SSE responses and
// large request bodies are never truncated — a caller that serves only short
// request/response pairs may set those separately. Existing non-zero values
// are preserved, so a caller can opt into stricter limits before calling.
func HardenServer(s *http.Server) {
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = serverReadHeaderTimeout
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = serverIdleTimeout
	}
}
