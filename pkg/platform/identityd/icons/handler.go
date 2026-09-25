package icons

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// LookupFn is the resolver surface NewHandler depends on.
type LookupFn interface {
	Lookup(ctx context.Context, credName string) Entry
}

var credNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// NewHandler returns an http.Handler serving GET /icon/<credName>.
//
// Deliberately public, no cookie gate: the cookie protects the rendered HTML,
// and an icon leaks no more than the operator-supplied SiteURL already on the
// CRD.
//
// Always 200 with bytes — a real favicon or the generated SVG fallback. 400 on
// a bad credName, 405 on non-GET, 304 on a matching If-None-Match.
func NewHandler(r LookupFn) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		credName := strings.TrimPrefix(req.URL.Path, "/icon/")
		if !credNameRegex.MatchString(credName) {
			http.Error(w, "invalid credential name", http.StatusBadRequest)
			return
		}
		e := r.Lookup(req.Context(), credName)

		if match := req.Header.Get("If-None-Match"); match != "" && match == e.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		// These bytes came from an UPSTREAM this deployment does not control,
		// and they are served on the origin that holds idd_session. The three
		// headers below make the response inert whatever it contains, and they
		// are independent of the type allowlist deliberately: the allowlist says
		// what may be fetched, this says what a browser may do with it, and a
		// future type added to the first must not silently re-open the second.
		//
		//   - nosniff: without it a mislabelled body is sniffed back into
		//     something executable, so the Content-Type check alone decides
		//     nothing.
		//   - CSP sandbox with no allow-* token: a top-level navigation to this
		//     URL gets an OPAQUE origin, so any script in it cannot reach
		//     idd_session or any same-origin route. default-src 'none' stops it
		//     fetching anything of its own.
		//   - Content-Disposition: attachment: a direct navigation downloads
		//     rather than renders. Subresource loads ignore this header, so the
		//     <img> case every real caller uses is unaffected.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("Content-Type", e.ContentType)
		if e.ETag != "" {
			w.Header().Set("ETag", e.ETag)
		}
		maxAge := 24 * time.Hour
		if e.Negative {
			maxAge = time.Hour
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(maxAge.Seconds())))
		_, _ = w.Write(e.Bytes)
	})
}
