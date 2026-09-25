package adminui

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// pinOrigin wraps a handler so every STATE-CHANGING method must arrive with the
// trusted Origin header.
//
// The framework ships this pin (webui.TrustedOriginMatch) and seven other
// surfaces use it; adminui used it at none of them. Its mutating routes kill a
// session, install an agent from a registry, and submit channel credentials —
// all cookie-authenticated POSTs with nothing binding them to this origin.
//
// GET is left alone: those routes are reads, and a top-level navigation sends
// no Origin header at all, so pinning them would refuse the SPA itself.
//
// Defense in depth rather than the whole story — the shipped mcp-ui client
// sandboxes inline widgets to an opaque origin and artifact HTML is inert under
// default-src 'none', so reaching these routes needs an external same-site
// scripting flaw. It is still the convention this codebase follows everywhere
// else, and following it here costs one wrapper.
func pinOrigin(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !webui.TrustedOriginMatch(r, d.TrustedOrigin()) {
			d.Logger().Info("adminui: origin mismatch on a state-changing admin route",
				"path", r.URL.Path, "method", r.Method, "origin", r.Header.Get("Origin"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"origin not trusted"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// proxyHandler reverse-proxies /admin/api/<rest> → <admind>/admin/v1/<rest>,
// attaching the service token + the authenticated subject. FlushInterval -1
// streams SSE through unbuffered. The admind token never reaches the
// browser — it is injected here, server-side, after the route's SpiceDB
// Authorize has already passed.
func proxyHandler(d Deps) http.Handler {
	target, err := url.Parse(d.AdmindBaseURL())
	if err != nil {
		// Misconfiguration: log loudly and serve 502s rather than panic.
		d.Logger().Info("adminui: invalid ADMIND_URL", "url", d.AdmindBaseURL(), "err", err.Error())
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"admin backend misconfigured; check ADMIND_URL"}`, http.StatusBadGateway)
		})
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The admin's own credentials do not travel to admind.
			//
			// Out starts as a clone of In, so the browser's session Cookie and
			// any inbound Authorization rode the internal hop to the operator
			// — which neither needs nor reads them. admind authenticates on the
			// bearer set below and identifies the caller from X-Admin-Subject;
			// a session cookie reaching it is a credential handed to a
			// component that has no use for it, which is the only kind of
			// credential exposure with no upside at all.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			// Built from admind's own constants, not from two literals: the
			// rewrite and the routes it fronts have to agree, and a spelling
			// repeated here would drift silently into an App created against a
			// callback that 404s.
			pr.Out.URL.Path = admind.APIPathPrefix + strings.TrimPrefix(pr.In.URL.Path, admind.BrowserAPIPathPrefix)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Header.Set("Authorization", "Bearer "+d.AdmindToken())
			pr.Out.Header.Set("X-Admin-Subject", webui.SubjectFromContext(pr.In.Context()))
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			d.Logger().Info("adminui: proxy to admind failed", "path", r.URL.Path, "err", err.Error())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"admin backend unreachable; see webd logs"}`))
		},
	}
	return rp
}
