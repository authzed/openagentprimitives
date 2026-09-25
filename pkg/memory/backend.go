package memory

import "context"

// Capabilities declares what a Backend can answer. The facade never lifts
// behavior beyond what the backend declares — predicates outside the declared
// capabilities surface as QueryResult.DroppedPredicates entries.
type Capabilities struct {
	// ContentSchemas: honors Kind.ContentSchema() for FieldEquals predicates.
	ContentSchemas bool
	// FieldRange: honors FieldFilter.Op values other than Eq (Lt, Lte, Gt, Gte,
	// IsNil, NotNil). Backends reporting false silently move non-Eq FieldFilter
	// predicates into DroppedPredicates.
	FieldRange bool
	// LinkTraversal: max hop depth. 0 = no link queries, 1 = one-hop,
	// -1 = unbounded.
	LinkTraversal int
	// ReverseLinks: can answer "what links TO this entry?".
	ReverseLinks bool
	// TagFilters: honors Query.Tags.
	TagFilters bool
	// TimeRange: honors Query.Since / Query.Until.
	TimeRange bool
}

// Backend is the storage primitive every backend implementation
// satisfies. Signal dispatch is framework concern (Memory.SendSignal),
// not backend concern — backends never receive Signals.
type Backend interface {
	Capabilities() Capabilities

	Put(ctx context.Context, e Entry) error
	Get(ctx context.Context, scope Scope, kind, id string) (Entry, bool, error)
	Delete(ctx context.Context, scope Scope, kind, id string) error
	// DeleteScope removes every entry under scope. Idempotent — an
	// empty scope is not an error.
	DeleteScope(ctx context.Context, scope Scope) error

	Query(ctx context.Context, q Query) (QueryResult, error)
	// QueryAllScopes is Query without the single-scope restriction; see
	// CrossScopeQuery. Backends must apply ordering before Offset/Limit.
	QueryAllScopes(ctx context.Context, q CrossScopeQuery) (QueryResult, error)
	Status(ctx context.Context, scope Scope) (ScopeStatus, error)
}
