// Dev/test override that redirects every `user:*` subject to a fixed canonical
// id before delegating to the real Writer.
//
// The approval flow is gated by SpiceDB relationships reflecting real-world
// ownership, so exercising it end-to-end on a dev cluster otherwise requires
// actually being the upstream owner. Configured with an OverrideCanonical (a
// base64-encoded user id), this rewrites every tuple whose subject carries the
// `user:` type prefix to `user:<override>`, letting one person play both
// requester and approver. Non-user subjects pass through unchanged.
//
// NON-PRODUCTION ONLY. While enabled, who-owns-what is silently false and
// approvals route to the override rather than the real owner. internal/cmd/runner logs a
// WARN at startup when the env var is set, and each rewrite logs at Info, so an
// operator who left it on has something to grep for.
package relwrites

import (
	"context"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// UserSubjectOverrideWriter wraps a Writer and rewrites every tuple
// whose subject is of type `user:` to use a fixed canonical id. See
// the package docstring for motivation + caveats.
type UserSubjectOverrideWriter struct {
	Inner Writer
	// OverrideCanonical is the base64-encoded user id to substitute
	// (the second half of "user:<canonical>"). Empty disables the
	// override and the writer becomes a pure pass-through.
	OverrideCanonical identity.CanonicalUserID
	// LogFn is the structured log sink — set to slog.Info or a
	// controller-runtime logger by the caller. nil = silent (still
	// rewrites, just no per-write log).
	LogFn func(msg string, kv ...any)
}

// WriteRelationships rewrites user-typed subjects then delegates.
func (w *UserSubjectOverrideWriter) WriteRelationships(ctx context.Context, tuples []ResolvedTuple) error {
	if w == nil || w.Inner == nil {
		return nil
	}
	if w.OverrideCanonical.IsZero() {
		return w.Inner.WriteRelationships(ctx, tuples)
	}
	rewritten := make([]ResolvedTuple, 0, len(tuples))
	for _, t := range tuples {
		orig := t.Subject
		if strings.HasPrefix(orig, "user:") {
			t.Subject = "user:" + w.OverrideCanonical.String()
			if w.LogFn != nil && orig != t.Subject {
				w.LogFn("relwrites: dev-override applied",
					"resource", t.Resource,
					"relation", t.Relation,
					"originalSubject", orig,
					"overrideSubject", t.Subject,
				)
			}
		}
		rewritten = append(rewritten, t)
	}
	return w.Inner.WriteRelationships(ctx, rewritten)
}
