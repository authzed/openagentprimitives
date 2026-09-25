package shadow

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	ReadFromPrimary   = "primary"
	ReadFromSecondary = "secondary"
)

type ctxKey struct{}

// WithReadFrom returns a context that overrides the shadow backend's
// read source for the duration of the request.
func WithReadFrom(ctx context.Context, readFrom string) context.Context {
	return context.WithValue(ctx, ctxKey{}, readFrom)
}

// ReadFromContext returns the per-request readFrom override, or "".
func ReadFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

// Backend dual-writes every mutation to both halves and serves reads from the
// one named by readFrom.
//
// The secondary is the DURABLE half (the operator wires primary=inmem,
// secondary=postgres), so its write errors are authoritative and reach the
// caller under BOTH read sources. readFrom selects where reads are served, not
// which half is durable: swallowing a secondary write error under
// readFrom=primary is still silent permanent loss, because the ephemeral primary
// is gone on the next pod roll.
type Backend struct {
	primary   memory.Backend
	secondary memory.Backend
	readFrom  string
	logger    logr.Logger
}

var _ memory.Backend = (*Backend)(nil)

func New(primary, secondary memory.Backend, readFrom string, logger logr.Logger) (*Backend, error) {
	if readFrom != ReadFromPrimary && readFrom != ReadFromSecondary {
		return nil, fmt.Errorf("shadow: invalid readFrom %q (must be %q or %q)",
			readFrom, ReadFromPrimary, ReadFromSecondary)
	}
	return &Backend{
		primary:   primary,
		secondary: secondary,
		readFrom:  readFrom,
		logger:    logger,
	}, nil
}

func (b *Backend) readerCtx(ctx context.Context) memory.Backend {
	rf := ReadFromContext(ctx)
	if rf == "" {
		rf = b.readFrom
	}
	if rf == ReadFromSecondary {
		return b.secondary
	}
	return b.primary
}

func (b *Backend) Capabilities() memory.Capabilities {
	if b.readFrom == ReadFromSecondary {
		return b.secondary.Capabilities()
	}
	return b.primary.Capabilities()
}

func (b *Backend) Put(ctx context.Context, e memory.Entry) error {
	if err := b.primary.Put(ctx, e); err != nil {
		return err
	}
	if secErr := b.secondary.Put(ctx, e); secErr != nil {
		// Logged as well as returned: the log records the DIVERGENCE (the
		// primary accepted what the durable half rejected), which the
		// returned error alone does not tell an operator reading logs.
		b.logger.Info("shadow: durable secondary Put failed, returning error to caller (primary accepted)",
			"scopeKind", e.Scope.Kind, "scopeID", e.Scope.ID,
			"kind", e.Kind, "id", e.ID, "readFrom", b.readFrom, "err", secErr.Error())
		return fmt.Errorf("shadow: durable secondary Put %s/%s in scope %s: %w",
			e.Kind, e.ID, e.Scope.ID, secErr)
	}
	return nil
}

func (b *Backend) Get(ctx context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	return b.readerCtx(ctx).Get(ctx, scope, kind, id)
}

func (b *Backend) Delete(ctx context.Context, scope memory.Scope, kind, id string) error {
	if err := b.primary.Delete(ctx, scope, kind, id); err != nil {
		return err
	}
	if secErr := b.secondary.Delete(ctx, scope, kind, id); secErr != nil {
		b.logger.Info("shadow: durable secondary Delete failed, returning error to caller (primary applied)",
			"scopeKind", scope.Kind, "scopeID", scope.ID,
			"kind", kind, "id", id, "readFrom", b.readFrom, "err", secErr.Error())
		return fmt.Errorf("shadow: durable secondary Delete %s/%s in scope %s: %w",
			kind, id, scope.ID, secErr)
	}
	return nil
}

func (b *Backend) DeleteScope(ctx context.Context, scope memory.Scope) error {
	if err := b.primary.DeleteScope(ctx, scope); err != nil {
		return err
	}
	if secErr := b.secondary.DeleteScope(ctx, scope); secErr != nil {
		b.logger.Info("shadow: durable secondary DeleteScope failed, returning error to caller (primary applied)",
			"scopeKind", scope.Kind, "scopeID", scope.ID,
			"readFrom", b.readFrom, "err", secErr.Error())
		return fmt.Errorf("shadow: durable secondary DeleteScope %s: %w", scope.ID, secErr)
	}
	return nil
}

func (b *Backend) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	return b.readerCtx(ctx).Query(ctx, q)
}

func (b *Backend) QueryAllScopes(ctx context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error) {
	return b.readerCtx(ctx).QueryAllScopes(ctx, q)
}

func (b *Backend) Status(ctx context.Context, scope memory.Scope) (memory.ScopeStatus, error) {
	return b.readerCtx(ctx).Status(ctx, scope)
}
