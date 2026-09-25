// Package debug builds the operator's PRODUCTION HTTP mux. The name misleads:
// this is load-bearing infrastructure enabled on every install, not a dev
// affordance. Do not disable it to "harden" a deployment; doing so takes the
// cluster down — the same address is the cluster-wide operatorURL every runner
// pod reads and writes memory through, the admin console is served nowhere
// else, and the operator's readyz gate blocks Ready until this listener
// accepts, so an operator without it never joins its Service.
//
// It is served on --debug-bind-address (default :8082, set in
// config/manager/deployment.yaml and so in the embedded install bundle).
// internal/cmd/operator mounts on it:
//
//   - /debug/artifact, /debug/artifacts — this package's own routes
//   - /healthz                          — unauthenticated liveness string
//   - /secret-output/                   — secretoutsrv
//   - /admin/                           — admind, the platform admin console
//   - /                                 — catch-all forward to the memory
//     handler (pkg/memory/httpsrv), which serves /memory/, /artifact/,
//     /artifact-bundle/ and /inbound-asset/
//
// Authentication is bearer-token, per route, and never ambient: the /debug/*
// routes take the global operator token (NewHandler) or, for artifacts, that
// token OR a per-session memory token whose authorized set covers the ref's
// session (NewHandlerWithMemAuth). Every handler the caller mounts
// authenticates its own requests — this mux adds no auth to what it forwards.
package debug

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
)

// NewHandler returns an http.Handler with endpoints:
//
//	GET /debug/artifact?ref=<url-encoded-ref>    — stream artifact bytes (requires bearer token)
//	GET /healthz                                  — "ok" (unauthenticated)
//
// token must be non-empty; requests without a matching "Authorization: Bearer <token>"
// header on /debug/* endpoints are rejected with 401.
func NewHandler(store artifactstore.Store, token string) *http.ServeMux {
	if token == "" {
		// Fail loudly rather than serve an accidentally-open handler.
		panic("debug.NewHandler: token must be non-empty")
	}
	mux := http.NewServeMux()
	mux.Handle("/debug/artifact", requireBearer(token, artifactHandler(store)))
	mux.Handle("/debug/artifacts", requireBearer(token, artifactListHandler(store)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func requireBearer(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authz, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spicebox-debug"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		provided := []byte(strings.TrimPrefix(authz, prefix))
		if subtle.ConstantTimeCompare(provided, expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spicebox-debug"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewHandlerWithMemAuth returns the standard debug handler with the memory
// extra mounted, AND wires the per-session token registry into the artifact
// handler's auth so runner pods can fetch their own session's artifacts.
func NewHandlerWithMemAuth(store artifactstore.Store, token string, extra http.Handler, reg *tokens.Registry) *http.ServeMux {
	if token == "" {
		panic("debug.NewHandlerWithMemAuth: token must be non-empty")
	}
	mux := http.NewServeMux()
	mux.Handle("/debug/artifact", artifactDualAuthHandler(store, token, reg))
	mux.Handle("/debug/artifacts", requireBearer(token, artifactListHandler(store)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	if extra != nil {
		// extra authenticates every request it serves, so this wrapper only has
		// to deliver each request with its original URL intact.
		//
		// A single catch-all, not one Handle per prefix extra happens to serve:
		// a hand-maintained list couples this file to httpsrv's route set by
		// name, and any prefix left off would 404 at THIS mux — indistinguishable
		// from the route not existing, since ServeMux never falls through once
		// nothing matches. "/" cannot shadow the exact routes above or the
		// subtrees the caller mounts afterward: ServeMux picks the longest
		// matching pattern, independent of registration order.
		mux.Handle("/", extra)
	}
	return mux
}

func artifactDualAuthHandler(store artifactstore.Store, debugToken string, reg *tokens.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			http.Error(w, "missing ref query parameter", http.StatusBadRequest)
			return
		}
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authz, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spicebox-debug"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		provided := []byte(strings.TrimPrefix(authz, prefix))

		// Path A: global debug token.
		if subtle.ConstantTimeCompare(provided, []byte(debugToken)) == 1 {
			serveArtifact(w, r, store, ref)
			return
		}

		// Path B: per-session memory token whose authorized-session set
		// includes the ref's path. The set covers the runner's AgentSession
		// path plus any per-bundle SpiceboxSession paths the operator
		// registered alongside it.
		if reg != nil {
			if ns, sess, ok := refSessionKey(store, ref); ok &&
				reg.Authorizes(string(provided), memory.NamespacedName{Namespace: ns, Name: sess}) {
				serveArtifact(w, r, store, ref)
				return
			}
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="spicebox-debug"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// refSessionKey derives (namespace, session) from an artifact ref by asking
// the store for the ref's internal key — keys are laid out
// "<ns>/<session>/…" by the toolcall controller — so per-session-token auth
// works regardless of the store's base URL (mem://, file:///path, gs://bucket).
func refSessionKey(store artifactstore.Store, ref string) (namespace, session string, ok bool) {
	key, err := store.Key(artifactstore.Ref(ref))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func serveArtifact(w http.ResponseWriter, r *http.Request, store artifactstore.Store, ref string) {
	rc, err := store.Get(r.Context(), artifactstore.Ref(ref))
	if err != nil {
		if err == artifactstore.ErrNotFound {
			http.Error(w, fmt.Sprintf("artifact not found: %s", ref), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("get artifact: %v", err), http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	// Artifact bytes are opaque payload, never a document this origin wants a
	// browser to interpret, so nosniff stops a sniffer promoting them to HTML.
	// Defence in depth: the route is bearer-only with no cookie auth, so no
	// browser reaches it today — this keeps both artifact writers matched.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		// Connection died mid-stream; nothing to do.
		_ = err
	}
}

type artifactListItem struct {
	// Ref is the opaque reference callers pass back to /debug/artifact.
	Ref string `json:"ref"`
	// Key is the store-internal object key, laid out "<ns>/<session>/…".
	Key string `json:"key"`
	// Size of the stored artifact, in bytes.
	Size int64 `json:"size"`
	// CreatedAt is the store timestamp, RFC3339 in UTC.
	CreatedAt string `json:"createdAt"`
}

type artifactListResponse struct {
	Items []artifactListItem `json:"items"`
	// Continue is the token for the next page; empty when the listing is done.
	Continue string `json:"continue"`
}

func artifactListHandler(store artifactstore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		prefix := q.Get("prefix")
		contToken := q.Get("continue")

		limit := 100
		if ls := q.Get("limit"); ls != "" {
			n, err := strconv.Atoi(ls)
			if err != nil || n < 0 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			if n > 0 {
				limit = n
			}
		}
		if limit > 500 {
			limit = 500
		}

		items, next, err := store.List(r.Context(), artifactstore.ListOpts{
			Prefix:   prefix,
			Limit:    limit,
			Continue: contToken,
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("list artifacts: %v", err), http.StatusInternalServerError)
			return
		}

		resp := artifactListResponse{
			Items:    make([]artifactListItem, len(items)),
			Continue: next,
		}
		for i, item := range items {
			resp.Items[i] = artifactListItem{
				Ref:       string(item.Ref),
				Key:       item.Key,
				Size:      item.Size,
				CreatedAt: item.CreatedAt.Format("2006-01-02T15:04:05Z"),
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func artifactHandler(store artifactstore.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			http.Error(w, "missing ref query parameter", http.StatusBadRequest)
			return
		}
		rc, err := store.Get(r.Context(), artifactstore.Ref(ref))
		if err != nil {
			if err == artifactstore.ErrNotFound {
				http.Error(w, fmt.Sprintf("artifact not found: %s", ref), http.StatusNotFound)
				return
			}
			http.Error(w, fmt.Sprintf("get artifact: %v", err), http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
	}
}
