// Package sessionevents serves trusted component event ingress. Session tokens
// cannot manufacture observations or read the private evidence retained here.
package sessionevents

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Server struct {
	Ingester *sessionevents.Ingester
	Tokens   *tokens.Registry
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || s.Tokens == nil || !s.Tokens.IsChannelsdToken(bearer) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.Ingester == nil {
		http.Error(w, "event ingress unavailable", http.StatusServiceUnavailable)
		return
	}
	kind := strings.TrimPrefix(r.URL.Path, "/session-events/")
	if kind == "" || strings.Contains(kind, "/") {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*sessionevents.MaxDataBytes))
	if err != nil {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	ctx := memory.WithCaller(memory.WithSystemApproval(r.Context(), "system:operator"), "system:operator")
	observation, err := s.Ingester.Ingest(ctx, kind, raw)
	if err != nil {
		log.FromContext(ctx).Info("component observation rejected", "kind", kind, "error", err)
		code := http.StatusServiceUnavailable
		if errors.Is(err, sessionevents.ErrInvalid) || errors.Is(err, sessionevents.ErrDenied) || errors.Is(err, sessionevents.ErrConflict) {
			code = http.StatusBadRequest
		} else {
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, "event rejected", code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		ID string `json:"id"`
	}{observation.ID()}); err != nil {
		log.FromContext(ctx).Info("event acknowledgment failed", "observation", observation.ID(), "error", err)
	}
}
