package browsergate

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GateHandler is the runner-facing listener: /healthz (unauthenticated) for the
// pod's startup probe, and an authenticated reverse proxy to Chrome's DevTools
// for everything else. The token must be presented before ANY request but
// /healthz reaches Chrome.
func (g *Gate) GateHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}

		if !g.authOK(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// The token must never reach Chrome; strip it from the forwarded URL.
		// (The /json/* rewrite below reads the token from config, not the query,
		// so stripping first is safe.)
		stripTokenParam(r)

		switch r.URL.Path {
		case "/json/version", "/json/list":
			g.serveJSONRewrite(w, r)
		default:
			g.devtoolsProxy.ServeHTTP(w, r)
		}
	})
}

// authOK reports whether r carries the per-session token, via an
// "Authorization: Bearer <token>" header OR a "?t=<token>" query param
// (websocket clients cannot set headers). The comparison is constant-time.
func (g *Gate) authOK(r *http.Request) bool {
	var presented string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		presented = strings.TrimPrefix(h, "Bearer ")
	} else {
		presented = r.URL.Query().Get("t")
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(g.cfg.Token)) == 1
}

// stripTokenParam removes the "t" token param from r.URL so it is not forwarded
// upstream to Chrome.
func stripTokenParam(r *http.Request) {
	q := r.URL.Query()
	if !q.Has("t") {
		return
	}
	q.Del("t")
	r.URL.RawQuery = q.Encode()
}

// serveJSONRewrite proxies /json/version and /json/list, rewriting every
// webSocketDebuggerUrl/devtoolsFrontendUrl so its upstream host is replaced with
// the gate's own Host header and the per-session token is appended. A chromedp
// NewRemoteAllocator pointed at the gate reads these URLs to connect, so this is
// what keeps it on the gate (authenticated) rather than dialing Chrome directly.
func (g *Gate) serveJSONRewrite(w http.ResponseWriter, r *http.Request) {
	target := *g.devtools
	target.Path = r.URL.Path

	resp, err := g.devtoolsCli.Get(target.String())
	if err != nil {
		g.logger.Info("browsergate: devtools /json fetch failed", "path", r.URL.Path, "err", err.Error())
		http.Error(w, "devtools upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		g.logger.Info("browsergate: devtools /json read failed", "path", r.URL.Path, "err", err.Error())
		http.Error(w, "devtools upstream error", http.StatusBadGateway)
		return
	}

	rewritten := g.rewriteJSON(body, r.Host)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)
}

// rewriteJSON parses body as a DevTools /json document and rewrites the
// debugger-URL fields in place. If the body does not parse as JSON it falls back
// to a raw host replacement (and logs): the resulting URLs then point at the
// gate but carry no token, so a client using them is refused (401) rather than
// handed a direct, unauthenticated path to Chrome — fail-closed.
func (g *Gate) rewriteJSON(body []byte, gateHost string) []byte {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		g.logger.Info("browsergate: devtools /json not valid JSON; host-replacing only", "err", err.Error())
		return []byte(strings.ReplaceAll(string(body), g.devtools.Host, gateHost))
	}
	rewriteNode(doc, g.devtools.Host, gateHost, g.cfg.Token)
	out, err := json.Marshal(doc)
	if err != nil {
		g.logger.Info("browsergate: devtools /json re-marshal failed; host-replacing only", "err", err.Error())
		return []byte(strings.ReplaceAll(string(body), g.devtools.Host, gateHost))
	}
	return out
}

// rewriteNode walks a decoded JSON value and rewrites every string value under a
// "webSocketDebuggerUrl" or "devtoolsFrontendUrl" key.
func rewriteNode(n any, upstreamHost, gateHost, token string) {
	switch v := n.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "webSocketDebuggerUrl" || k == "devtoolsFrontendUrl" {
				if s, ok := val.(string); ok {
					v[k] = rewriteDebuggerURL(s, upstreamHost, gateHost, token)
					continue
				}
			}
			rewriteNode(val, upstreamHost, gateHost, token)
		}
	case []any:
		for _, item := range v {
			rewriteNode(item, upstreamHost, gateHost, token)
		}
	}
}

// rewriteDebuggerURL replaces the upstream host with the gate host and appends
// the token as a "t" query param (with the correct ? or & separator).
func rewriteDebuggerURL(s, upstreamHost, gateHost, token string) string {
	s = strings.ReplaceAll(s, upstreamHost, gateHost)
	sep := "?"
	if strings.Contains(s, "?") {
		sep = "&"
	}
	return s + sep + "t=" + url.QueryEscape(token)
}
