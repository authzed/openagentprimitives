// grantWriter adapts a RelWriter to the option-free Writer surface used by
// pkg/authz/guardian/grants. RelWriter's WriteRelationships/
// DeleteRelationships are already option-free, so this adapter's only job
// now is the same nil-preserving shape it always had: NewGrantWriter(nil)
// must yield a nil *GrantWriter, not a non-nil wrapper around nil (see
// AGENTS.md "Nil interfaces: never assign a typed-nil pointer"). Centralized
// here so the runner and channelsd both adapt against the same code instead
// of each maintaining their own copy.
package spicedb

import (
	"context"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
)

// GrantWriter wraps a RelWriter for pkg/authz/guardian/grants.Writer (and any
// other no-option Writer-shaped interface) without forcing this package to
// import the higher-level guardian/grants package (which would create a
// cycle if grants ever needed transport-level helpers from
// pkg/authz/spicedb).
type GrantWriter struct {
	w RelWriter
}

// NewGrantWriter wraps w. Returns nil when w is nil so callers can guard
// conditional wiring (e.g. `pl.GrantWriter = spicedb.NewGrantWriter(c.Writer(src))`)
// without producing a non-nil interface backed by a nil pointer — see
// AGENTS.md "Nil interfaces: never assign a typed-nil pointer".
func NewGrantWriter(w RelWriter) *GrantWriter {
	if w == nil {
		return nil
	}
	return &GrantWriter{w: w}
}

// WriteRelationships forwards to the wrapped RelWriter.
func (g *GrantWriter) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	return g.w.WriteRelationships(ctx, req)
}

// DeleteRelationships forwards to the wrapped RelWriter.
func (g *GrantWriter) DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	return g.w.DeleteRelationships(ctx, req)
}
