package hooks

import (
	"context"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation/kinds/toolorigin"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// RevocationGuardDeps wires the revocation PreToolCall guard.
type RevocationGuardDeps struct {
	// Set is the live revoked-origin set consulted on each call.
	Set *toolorigin.Set
	// LookupOrigin maps an LLM tool name to its Origin() string (e.g.
	// "mcpserver/x", "sidecartoolbox/y", "toolkit/z"). Returns "" for
	// origin-less tools (sandbox, meta); those are never revoked.
	LookupOrigin func(toolName string) string
	// Logger is nil-safe; defaults to slog.Default().
	Logger *slog.Logger
}

// RevocationGuard is the PreToolCall hook that denies calls whose tool origin
// appears in the live revoked-origin set.
type RevocationGuard struct{ d RevocationGuardDeps }

// NewRevocationGuard builds the guard. Logger defaults to slog.Default() when nil.
func NewRevocationGuard(d RevocationGuardDeps) *RevocationGuard {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &RevocationGuard{d: d}
}

func (h *RevocationGuard) Name() string             { return "revocation_guard" }
func (h *RevocationGuard) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }

func (h *RevocationGuard) Eval(ctx context.Context, in pipeline.Input) pipeline.Decision {
	if in.Tool == nil {
		return pipeline.Decision{}
	}
	origin := h.d.LookupOrigin(in.Tool.Name)
	if origin == "" {
		return pipeline.Decision{}
	}
	if !h.d.Set.IsRevoked(origin) {
		return pipeline.Decision{}
	}
	h.d.Logger.Info("revocation: tool denied; origin revoked",
		"session", in.Session.String(), "tool", in.Tool.Name, "origin", origin)
	return pipeline.Decision{
		Verdict: pipeline.Deny,
		Reason:  "revoked: " + origin,
	}
}

var _ pipeline.Hook = (*RevocationGuard)(nil)
