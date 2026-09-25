//go:build e2e

// pkg/platform/identityd/e2e_seam.go — e2e-only test seams.
//
// An e2e scenario runs in a separate package and points identityd's OAuth
// handlers at an in-process fakeoauth httptest.Server. The production
// safehttp.Client correctly refuses the loopback address httptest binds, which
// would block discovery + DCR + token exchange — hence the override below.
//
// Built only under -tags=e2e, so production binaries never see the symbol and
// the SSRF guard on the production path stays untouched.
package identityd

import "net/http"

// SetOAuthHTTPClientForTesting replaces the package-level OAuth HTTP client
// factory with one returning hc, and returns a restore function to defer.
func SetOAuthHTTPClientForTesting(hc *http.Client) (restore func()) {
	prev := newOAuthHTTPClient
	newOAuthHTTPClient = func() *http.Client { return hc }
	return func() { newOAuthHTTPClient = prev }
}
