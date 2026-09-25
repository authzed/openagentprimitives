package artifactstore

import (
	"context"
	"errors"
	"io"
	"time"
)

// Ref is an opaque, scheme-prefixed reference to a stored artifact
// (e.g., "mem://ns/session/call/stream", "s3://bucket/key").
type Ref string

// Store is the artifact storage abstraction used by the operator for
// stdin/stdout/stderr and input/output artifact handoff.
type Store interface {
	// Put writes r's bytes under key and returns an opaque Ref.
	// Implementations must read r to EOF before returning.
	Put(ctx context.Context, key string, r io.Reader) (Ref, error)

	// Get returns a ReadCloser for the contents at ref.
	// Returns ErrNotFound if ref is unknown.
	Get(ctx context.Context, ref Ref) (io.ReadCloser, error)

	// Delete removes the artifact. Idempotent: no error if already gone.
	Delete(ctx context.Context, ref Ref) error

	// Exists is a cheap presence check.
	Exists(ctx context.Context, ref Ref) (bool, error)

	// List returns artifacts matching opts, with an opaque continue token
	// when more pages exist. Returns "" for next when the page is the last.
	List(ctx context.Context, opts ListOpts) (items []Item, next string, err error)

	// Key returns the internal storage key a Ref of this store resolves to —
	// the inverse of Put's Ref construction. Foreign refs (a different
	// scheme, bucket, or base path) return an error naming both bases.
	Key(ref Ref) (string, error)
}

// Item is a single artifact listing entry.
type Item struct {
	Ref       Ref
	Key       string
	Size      int64
	CreatedAt time.Time
}

// ListOpts controls filtering and pagination for List.
type ListOpts struct {
	Prefix   string // optional; only refs whose internal key starts with Prefix
	Limit    int    // 0 → server default (100)
	Continue string // opaque continuation token from prior call
}

// ErrNotFound is returned by Get/Exists when a Ref doesn't resolve.
var ErrNotFound = errors.New("artifactstore: not found")
