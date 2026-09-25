package memory

import "context"

// Authorizer gates memory operations behind an external authorization system.
// When set on the Local facade, every Put/Query/Delete passes through it.
//
// It is NOT the front door: EnsureApproval at the top of each facade method is
// what enforces coarse read/write authorization, with or without an Authorizer
// configured. The Authorizer is the tuple-writer (AuthorizePut records
// entry-level relationships for future checks) plus an optional finer per-entry
// filter, reached only once the door has already let the call through.
//
// Implementations MUST be safe for concurrent use.
type Authorizer interface {
	// AuthorizePut runs AFTER the Local.Put door has already authorized the
	// write, so it is NOT itself the write-permission gate. Its job is to WRITE
	// the entry-level relationships (memory_entry#session, #creator, and
	// caller-independent ones like artifact#parent) so future reads/deletes can
	// be checked. A non-nil error — e.g. a failed relationship write — denies
	// the write and reaches the caller as-is.
	//
	// With CallerFrom(ctx) absent (in-process controller calls),
	// implementations SHOULD allow the write unconditionally.
	AuthorizePut(ctx context.Context, e Entry) error

	// AuthorizeQuery post-filters a backend result once the EnsureApproval door
	// has authorized the scope as a whole, returning only the entries the
	// caller (CallerFrom(ctx)) may read. Invoked only when a caller is present,
	// so it is a finer per-entry layer on the door, not the read gate itself.
	// Implementations bulk-check to minimize round-trips.
	AuthorizeQuery(ctx context.Context, entries []Entry) ([]Entry, error)

	// AuthorizeDelete runs before backend.Delete; a non-nil error denies the
	// deletion. With CallerFrom(ctx) absent, implementations SHOULD allow.
	AuthorizeDelete(ctx context.Context, scope Scope, kind, id string) error

	// CleanupScope idempotently removes all entry-level authorization
	// relationships for the scope.
	CleanupScope(ctx context.Context, scope Scope) error
}
