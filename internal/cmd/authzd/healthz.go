package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extracted_entity"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/extraction_state"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// healthState exposes /healthz and /readyz. Liveness is true if NATS
// connect succeeded; readiness flips true after the subscription is
// established.
type healthState struct {
	natsConnected atomic.Bool
	subActive     atomic.Bool
	// debugToken gates /debug/sessions. Empty means the route is not served
	// at all — see handler().
	debugToken string
	deps       struct {
		Memory memory.Memory
	}
}

func (h *healthState) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !h.natsConnected.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("nats not connected\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !h.subActive.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("subscription not active\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	// The debug route is served ONLY when a token is configured, and only to a
	// caller presenting it.
	//
	// It shares this mux, which is served on :8080 across all interfaces with
	// no TLS, and it reads through authzd's memory token — which the operator
	// treats as cluster-wide. Ungated it dumped ANY session's scope and the
	// span of text a candidate was extracted from, to anything that could
	// reach the pod. NetworkPolicy was the only thing in the way, and nothing
	// verifies the cluster's CNI enforces NetworkPolicy at all: stock EKS
	// without the policy add-on and GKE Standard without Dataplane V2 do not,
	// and there every pod — a tenant's sandbox included — could read it.
	//
	// Unset means NOT SERVED rather than open, because falling open is how a
	// debug affordance becomes a disclosure route in the first place.
	if h.debugToken != "" {
		mux.HandleFunc("/debug/sessions/", h.requireDebugToken(h.debugSessions))
	}
	return mux
}

// requireDebugToken wraps a debug handler in a constant-time bearer check.
//
// A failure answers 404 rather than 401: the route's existence is itself
// information, and an operator holding the token does not need to be told the
// route is there.
func (h *healthState) requireDebugToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(presented), []byte(h.debugToken)) != 1 {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// debugSessions exposes the memory store's view of a session's scope.
// URL: /debug/sessions/<ns>/<name>/bindings
// (path kept for backwards compat; response now shows session_scope resources)
func (h *healthState) debugSessions(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/debug/sessions/")
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[2] != "bindings" {
		http.NotFound(w, r)
		return
	}
	ns, name := parts[0], parts[1]
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}

	ctx := r.Context()
	var scopeResources any
	if h.deps.Memory != nil {
		sc, _, _ := sessionscope.Get(ctx, h.deps.Memory, scope)
		scopeResources = sc.Resources
	}
	extracted, _ := queryExtractedEntities(ctx, h.deps.Memory, scope)
	states, _ := queryExtractionStates(ctx, h.deps.Memory, scope)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"scope":            scope,
		"bindings":         scopeResources, // field kept for backwards-compat; now shows session_scope resources
		"extracted":        extracted,
		"extraction_state": states,
	})
}

func queryExtractedEntities(ctx context.Context, m memory.Memory, scope memory.Scope) ([]extracted_entity.Content, error) {
	if m == nil {
		return nil, nil
	}
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"extracted_entity"}})
	if err != nil {
		return nil, err
	}
	out := make([]extracted_entity.Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c extracted_entity.Content
		if json.Unmarshal(e.Content, &c) == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func queryExtractionStates(ctx context.Context, m memory.Memory, scope memory.Scope) ([]extraction_state.Content, error) {
	if m == nil {
		return nil, nil
	}
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{"extraction_state"}})
	if err != nil {
		return nil, err
	}
	out := make([]extraction_state.Content, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c extraction_state.Content
		if json.Unmarshal(e.Content, &c) == nil {
			out = append(out, c)
		}
	}
	return out, nil
}
