package memory

import (
	"math"
	"regexp"
	"time"
)

var safeFieldPathRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)

// SafeFieldPath reports whether path is safe to interpolate into a SQL
// expression as a JSON field accessor. A FieldFilter.Path reaches the SQL
// backends as an identifier, not a bind parameter, so this is the injection
// guard — never interpolate a path that fails it.
func SafeFieldPath(path string) bool { return safeFieldPathRe.MatchString(path) }

// OrderBy controls Query result ordering.
type OrderBy struct {
	// Field is the column to order by; "createdAt" is the only value any
	// backend honors, and anything else falls back to the backend's default.
	Field string
	// Desc: true for newest-first.
	Desc bool
}

// LinkFilter selects entries by one of their links.
type LinkFilter struct {
	// Relation to match; empty matches any relation.
	Relation string
	// Kind of the linked entry.
	Kind string
	// ID of the linked entry.
	ID string
}

// FieldOp is the comparison operator for a FieldFilter predicate. The zero
// value ("") means FieldOpEq, so an op-less filter is an equality filter.
type FieldOp string

const (
	FieldOpEq     FieldOp = "eq"
	FieldOpLt     FieldOp = "lt"
	FieldOpLte    FieldOp = "lte"
	FieldOpGt     FieldOp = "gt"
	FieldOpGte    FieldOp = "gte"
	FieldOpIsNil  FieldOp = "is_nil"
	FieldOpNotNil FieldOp = "not_nil"
)

// NormalizeFieldOp resolves the zero value to FieldOpEq, leaving any other op
// unchanged.
func NormalizeFieldOp(op FieldOp) FieldOp {
	if op == "" {
		return FieldOpEq
	}
	return op
}

// FieldFilter selects entries by a content field predicate. A backend without
// Capabilities.ContentSchemas drops it into QueryResult.DroppedPredicates at
// runtime; one without Capabilities.FieldRange drops non-Eq predicates the same
// way.
type FieldFilter struct {
	// Path is the key the value is STORED under — the Content struct's `json`
	// tag, NOT its Go field name. Backends render the predicate as
	// content->>'<Path>', so "TurnIndex" for a field tagged `json:"turnIndex"`
	// extracts NULL and can never be true. Local.Query rejects a Path naming no
	// content key of the Kinds queried, rather than answering empty forever.
	//
	// Path is a TOP-LEVEL key, not a nesting expression: both SQL backends read
	// a dot as part of the key name (postgres's jsonb ->> takes a literal key,
	// sqlite's ->> quotes the operand into $."a.b"), so "a.b" selects a key
	// literally named "a.b" and never reaches a nested {"a":{"b":…}}. Nested
	// content is not queryable. The Kind should also list the key in
	// IndexedFields().
	Path string `json:"path"`
	// Op is the comparison; empty means equality.
	Op FieldOp `json:"op,omitempty"`
	// Value is the right-hand side; ignored by the IsNil/NotNil ops.
	Value any `json:"value,omitempty"`
}

// Query is the rich superset of recall predicates. Backends drop what
// they can't handle into QueryResult.DroppedPredicates; callers always
// have a fallback path for empty / partial results.
type Query struct {
	Scope        Scope         // required; the framework rejects an empty one
	Kinds        []string      // empty means all kinds in scope
	IDs          []string      // exact-id filter
	LinkedTo     []LinkFilter  // entries whose Links contain {Relation?, Kind, ID}
	LinkedFrom   []LinkFilter  // reverse: entries that ARE linked-to {Kind, ID}
	Tags         []string      // entries carrying ALL the listed tags
	FieldEquals  []FieldFilter // content predicates; dropped by backends lacking ContentSchemas
	Since, Until *time.Time    // half-open CreatedAt window; nil means unbounded
	OrderBy      OrderBy
	Limit        int // 0 means the backend's own cap
}

// CrossScopeQuery matches entries of the given kinds across ALL scopes of
// ScopeKind, time-filtered and time-ordered. Unlike Query there is NO per-entry
// authorization post-filter — its only caller is the admind admin API, which
// gates on the SpiceDB platform permissions first. Do not expose it through any
// per-user surface.
type CrossScopeQuery struct {
	ScopeKind string     // required, e.g. "session"
	Kinds     []string   // required, non-empty
	Since     *time.Time // inclusive
	Until     *time.Time // exclusive
	OrderDesc bool       // true = newest first
	Limit     int        // 0 = no limit
	Offset    int        // rows to skip after ordering
}

// QueryResult is what Backend.Query returns.
type QueryResult struct {
	// Entries matched the honored predicates; empty is a valid answer, not an error.
	Entries []Entry
	// DroppedPredicates names each Query predicate the backend could not honor
	// ("FieldEquals", "LinkedFrom", …). A capability signal for operators;
	// callers must not branch on it for correctness.
	DroppedPredicates []string
	// Partial is true iff len(DroppedPredicates) > 0.
	Partial bool
	// Truncated reports that Query.Limit ended this read before the matches
	// did: at least one more entry matched and is not in Entries. False means
	// Entries is every match, so a caller may treat it as the whole answer.
	//
	// It is set by Local.Query, which probes one row past the limit (see
	// ProbeLimit) rather than guessing from len(Entries) — a set holding
	// exactly Limit entries is otherwise indistinguishable from one the limit
	// cut short, and that ambiguity is what lets a reader act on part of a
	// result set believing it was all of it.
	//
	// It describes the READ, not the caller's view of it: an Authorizer
	// post-filter may drop entries afterwards, leaving fewer than Limit in
	// Entries with Truncated still true. That is correct — the store held more
	// than the limit allowed to be read, whatever was then filtered out.
	//
	// Backend implementations leave it false; the facade owns it.
	Truncated bool
}

// ProbeLimit returns the row count to request from a store so a truncated
// answer stays distinguishable from a complete one: one more row than the
// caller asked for. The extra row is never rendered — only the fact that it
// existed is (see TrimProbe).
//
// An unlimited read (limit <= 0) needs no probe: nothing is discarded, so
// nothing can be hidden. A limit already at MaxInt is returned unchanged
// rather than wrapped negative, which a store would read as "unlimited" and
// answer with the whole scope.
func ProbeLimit(limit int) int {
	if limit <= 0 || limit == math.MaxInt {
		return limit
	}
	return limit + 1
}

// TrimProbe drops the extra row ProbeLimit asked for and reports whether it was
// there — that is, whether limit cut the answer short. Rows at or under the
// limit come back untouched with truncated false, so a listing that merely
// filled its page is never reported as one that was cut.
//
// It trims to limit even when a store over-answers the probe, so a caller can
// rely on the result honoring the limit it asked for whatever the store did.
func TrimProbe[T any](rows []T, limit int) ([]T, bool) {
	if limit <= 0 || len(rows) <= limit {
		return rows, false
	}
	return rows[:limit], true
}
