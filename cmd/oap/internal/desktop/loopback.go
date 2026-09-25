package desktop

import (
	"net"
	"net/http"
	"net/url"
)

// LoopbackGuard rejects requests whose Host (and, when present, Origin)
// header doesn't name a loopback address. Both the setup UI server and the
// settings UI server bind their listener to 127.0.0.1 only, so this is
// defense in depth, not the primary control: a browser tab left open on some
// other origin can still be pointed at http://127.0.0.1:<port>/... by page
// script (the classic "drive-by localhost" pattern) and would otherwise ride
// the loopback bind with a forged/foreign Origin. curl/CLI callers never send
// Origin at all, which this treats as fine — Origin is a browser-only
// header.
func LoopbackGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			originHost, err := hostOf(origin)
			if err != nil || !isLoopbackHost(originHost) {
				http.Error(w, "forbidden: loopback only", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostOf extracts the host[:port] component of a URL string (used for the
// Origin header, which is a full "scheme://host[:port]" with no path).
func hostOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return u.Host, nil
}

// isLoopbackHost reports whether host (an HTTP Host or Origin header's host,
// optionally with a ":port" suffix) names a loopback address.
func isLoopbackHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
