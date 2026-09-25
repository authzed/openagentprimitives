package memory

import "context"

type kgScopeKey struct{}

// WithKGScope threads the session scope a knowledge-graph read must be filtered
// to. The operator's per-session /memory/_kg handler sets it; the platform-admin
// (admind) cross-session browser deliberately does NOT, so its reads stay
// unscoped (it is gated on platform#view_audit instead).
func WithKGScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, kgScopeKey{}, s)
}

// KGScopeFrom returns the KG read scope and true when one is set with a non-empty
// ID; ok is false when unset or the ID is empty (the unscoped admin path).
func KGScopeFrom(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(kgScopeKey{}).(Scope)
	return s, ok && s.ID != ""
}
