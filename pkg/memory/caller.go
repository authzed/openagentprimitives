package memory

import "context"

type callerKey struct{}

// WithCaller records the USER on whose behalf the request runs, as a canonical
// subject id. Absent for component and per-session credentials — see
// TokenSessionFrom for the credential's own identity, which is a different
// question.
func WithCaller(ctx context.Context, canonicalID string) context.Context {
	return context.WithValue(ctx, callerKey{}, canonicalID)
}

// CallerFrom reports the user id and whether one is set. Unlike
// TokenSessionFrom, the bool folds emptiness in: an empty caller is genuinely
// "no user", the normal state for a runner or component write.
func CallerFrom(ctx context.Context) (string, bool) {
	c, ok := ctx.Value(callerKey{}).(string)
	return c, ok && c != ""
}

// WithoutCaller returns ctx with the caller mark cleared, so CallerFrom
// reports absent again. Mirrors WithoutTokenSession: because CallerFrom folds
// emptiness into its bool (see above), storing "" under the same key is
// sufficient — no change to CallerFrom's storage or contract is needed.
//
// For the same authorship hand-offs WithoutTokenSession exists for: a path
// that stops acting on a user's behalf and starts writing as the platform
// itself (SystemContext) must drop the mark, or the write would still be
// attributed to that user.
func WithoutCaller(ctx context.Context) context.Context {
	if _, ok := CallerFrom(ctx); !ok {
		return ctx
	}
	return WithCaller(ctx, "")
}
