package memory

import "context"

type tokenSessionKey struct{}

// WithTokenSession marks ctx as originating from a PER-SESSION bearer token
// issued for sess. It answers a different question from WithCaller and the two
// must not be conflated:
//
//   - CallerFrom is the USER on whose behalf the request runs. It is what
//     spicedbauthorizer writes as memory_entry#creator, and it is deliberately
//     absent for non-user callers — the AgentSession reconciler registers every
//     per-session memory token with an empty callerID, because a runner's write
//     has no user creator to record.
//   - TokenSessionFrom is the CREDENTIAL's own identity: which session's token
//     authenticated this request. It is present for exactly the per-session
//     bearers and absent for everything else, independent of whether a user is
//     attached.
//
// Provenance verification needs the second: an append-only entry is attributed
// to a publisher (session:<ns>/<name>), and the question at the door is "is the
// writer the publisher it claims to be", not "which user asked". Reusing
// CallerID would force it non-empty for runners, silently writing
// memory_entry#creator tuples for a subject that is not a user.
func WithTokenSession(ctx context.Context, sess NamespacedName) context.Context {
	return context.WithValue(ctx, tokenSessionKey{}, sess)
}

// TokenSessionFrom reports the session whose bearer token authenticated this
// request, and whether the request arrived on such a token at all.
//
// The bool is PRESENCE, not non-emptiness, and that distinction is the whole
// point: a security check written as `if caller != "" { … }` degrades silently
// to "no check" for the one caller class that never carries a caller. Treat a
// present-but-empty NamespacedName as a wiring bug and fail CLOSED, never as
// "not token-originated".
func TokenSessionFrom(ctx context.Context) (NamespacedName, bool) {
	sess, ok := ctx.Value(tokenSessionKey{}).(NamespacedName)
	return sess, ok
}

// WithoutTokenSession returns ctx with the token-session mark removed, so
// TokenSessionFrom reports absent again.
//
// For authorship HAND-OFFS only: a path that stops acting on the token holder's
// behalf and starts writing as the process itself (Local's ScopeHooks fan-out)
// must drop the mark, or the provenance writer-binding demands the process's own
// entries be attributed to the token holder. NOT a way to opt out of the binding
// for a write the token holder still authored.
func WithoutTokenSession(ctx context.Context) context.Context {
	if _, ok := TokenSessionFrom(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, tokenSessionKey{}, nil)
}

// SystemContext returns a child of ctx that acts as the named platform
// component: any per-session token identity and caller are cleared (via
// WithoutTokenSession / WithoutCaller) and a system approval is attached (via
// WithSystemApproval). For operator-internal reads/writes performed on behalf
// of the platform rather than the calling session — the user_preference kind's
// commit path is the first caller: it must author ComponentWritten entries in
// a user's scope without inheriting whatever session or caller mark the
// triggering request carried.
func SystemContext(ctx context.Context, component string) context.Context {
	ctx = WithoutTokenSession(ctx)
	ctx = WithoutCaller(ctx)
	return WithSystemApproval(ctx, component)
}
