// Package skillbundle is the content-addressed store for skill bundles (the
// gzipped tar of a skill directory: SKILL.md + scripts/ + assets/ + ...). It is
// keyed by content digest so a bundle is cached once per (repo, sha, skill).
// Two backends: memory/ (process-local) and postgres/ (durable, shared across
// pods).
package skillbundle

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get when a digest is not present in the store.
var ErrNotFound = errors.New("skillbundle: digest not found")

// Store persists skill bundle tarballs keyed by content digest.
type Store interface {
	// Put stores the gzipped-tar bytes under the digest (idempotent).
	Put(ctx context.Context, digest string, data []byte) error
	// Get returns the bytes for a digest, or ErrNotFound.
	Get(ctx context.Context, digest string) ([]byte, error)
	// Has reports whether a digest is present.
	Has(ctx context.Context, digest string) (bool, error)
}
