package scope

// ScopeDelta is the mutation applied to a Scope. Add widens; Remove
// narrows; HardDeny records explicit disallows into the Layer-2 memory
// Disallow / tool-deny sets, which the Scope hook enforces at dispatch. All
// three may be set in one delta; ApplyDelta folds Add, Remove, and HardDeny
// into the next Scope.
type ScopeDelta struct {
	// Add widens the scope; empty means this delta grants nothing new.
	Add ScopePartial `json:"add,omitzero"`
	// Remove narrows the scope; removing an absent entry is a no-op, not an error.
	Remove ScopePartial `json:"remove,omitzero"`
	// HardDeny blocks at dispatch even if some other rule would allow.
	HardDeny ScopePartial `json:"hardDeny,omitzero"`
}

// ScopePartial groups changes by axis. The same shape is used for
// Add/Remove/HardDeny so callers can compose deltas uniformly.
type ScopePartial struct {
	// Resources are concrete (type, id) references, already enumerated.
	Resources []ResourceRef `json:"resources,omitempty"`
	// ResourcePatterns are globs enumerated at apply time, never here.
	ResourcePatterns []ResourcePattern `json:"resourcePatterns,omitempty"`
	// Tools are tool names, exact or glob (e.g. "github.*").
	Tools []string `json:"tools,omitempty"`
	// ArgConstraints restrict specific argument values on a matched tool.
	ArgConstraints []ArgConstraint `json:"argConstraints,omitempty"`
}

// FlattenResources returns the concrete ResourceRef list. Patterns
// are NOT flattened here — pattern enumeration happens at apply
// time via LookupResources and is the caller's responsibility.
func (p ScopePartial) FlattenResources() []ResourceRef {
	out := make([]ResourceRef, 0, len(p.Resources))
	out = append(out, p.Resources...)
	return out
}

// IsEmpty reports whether the partial contains any change.
func (p ScopePartial) IsEmpty() bool {
	return len(p.Resources) == 0 &&
		len(p.ResourcePatterns) == 0 &&
		len(p.Tools) == 0 &&
		len(p.ArgConstraints) == 0
}

// IsZero is the encoding/json hook required when ScopeDelta fields
// use omitzero; it delegates to IsEmpty so callers have one
// authoritative emptiness predicate.
func (p ScopePartial) IsZero() bool { return p.IsEmpty() }

// IsEmpty reports whether the entire delta is a no-op.
func (d ScopeDelta) IsEmpty() bool {
	return d.Add.IsEmpty() && d.Remove.IsEmpty() && d.HardDeny.IsEmpty()
}

// Shape classifies a delta as "widen", "narrow", "mixed", or "" (empty),
// derived post-LLM by the metaagent Extract stage from the parsed delta:
//
//   - Add present, no Remove/HardDeny ⇒ "widen" (grant new access)
//   - only Remove/HardDeny, no Add    ⇒ "narrow" (revoke / hard-deny)
//   - both                            ⇒ "mixed"
//   - empty                           ⇒ ""
//
// Whether a request widens or narrows is the MEANING of the untrusted text,
// so it is unknowable at Received (pre-LLM) and is derived here from the
// extractor's parsed delta. Pure; no side effects.
func (d ScopeDelta) Shape() string {
	if d.IsEmpty() {
		return ""
	}
	widens := !d.Add.IsEmpty()
	narrows := !d.Remove.IsEmpty() || !d.HardDeny.IsEmpty()
	switch {
	case widens && narrows:
		return "mixed"
	case widens:
		return "widen"
	default:
		return "narrow"
	}
}
