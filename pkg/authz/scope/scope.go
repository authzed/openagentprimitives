// Package scope is the structured per-session permission policy document —
// Layer 2 of dynamic session scope. Pure data types and helpers, no external
// deps.
package scope

import (
	"fmt"
	"strings"
	"time"
)

// Source tags where a ScopeResource entry came from. Used for audit
// and for migration projections (e.g., projecting default-source
// resources to the legacy binding view).
type Source string

const (
	SourceDefault           Source = "default"
	SourceInitialAsk        Source = "initial-ask"
	SourceMetaagentApproved Source = "metaagent-approved"
	SourceExtracted         Source = "extracted"
	SourceApproverDenyConv  Source = "approver-deny-converted"

	// SourceApproved is a binding a HUMAN cleared — a plan-gate slot approval
	// or a JIT tool approval.
	//
	// Its own value rather than reuse of SourceDefault, because the audit trail
	// is the only place the difference survives: `default` came from class
	// config, `extracted` from text the agent read, and `approved` from a
	// person who was shown the resource and said yes. Collapsing them would
	// make a human decision indistinguishable from configuration.
	SourceApproved Source = "approved"

	// SourceObserved is a binding proposed by a recorded FACT: an object some
	// observation named, bound because the requester already had standing on
	// it.
	//
	// Its own value for the reason SourceApproved states. `extracted` came out
	// of an LLM reading the user's prose; `observed` came out of a signed
	// envelope or a declared CEL expression over a tool result, with no model
	// anywhere in the derivation. An auditor asking "how did this instance get
	// into the session" is asking exactly that question, and the two answers
	// carry different weight.
	SourceObserved Source = "observed"
)

// Scope is the structured per-session permission policy document
// (Layer 2). One per AgentSession; persisted as memory Kind
// "session_scope" with stable ID "scp-config". Bumping ScopeVersion
// on every applied delta lets the runner invalidate caches.
type Scope struct {
	// Resources the session may reach; empty means no resource is in scope.
	Resources []ScopeResource `json:"resources,omitempty"`
	// Disallow is the hard-deny set. A tool call resolving to a resource that
	// matches any Disallow entry (by explicit ID or ID glob, e.g. "ENG-*") is
	// blocked at dispatch — independent of toolCalls.mode, so it holds even when
	// per-tool static authz is disabled. Populated from ScopeDelta.HardDeny.
	Disallow []ScopeResource `json:"disallow,omitempty"`
	// Tools narrows the invocable tool names; zero value imposes no restriction.
	Tools ScopeTools `json:"tools,omitzero"`
	// ArgConstraints restrict argument values per tool; empty means unconstrained.
	ArgConstraints []ArgConstraint `json:"argConstraints,omitempty"`
	// Expiry is when this scope stops applying; nil means it never expires.
	Expiry *time.Time `json:"expiry,omitempty"`
	// ScopeVersion increments on every applied delta so readers can invalidate caches.
	ScopeVersion int64 `json:"scopeVersion"`
}

// ResourceDisallowed reports whether resType:resID is hard-denied by this
// scope. An entry's ID matches either exactly or as a glob (filepath.Match),
// so "ENG-*" denies every linear_issue whose id starts with "ENG-". Used at
// tool dispatch as a fail-closed gate that runs regardless of toolCalls.mode.
func (s Scope) ResourceDisallowed(resType, resID string) bool {
	if resID == "" {
		return false
	}
	for _, d := range s.Disallow {
		if d.ResourceType != resType {
			continue
		}
		for _, idPat := range d.IDs {
			if idPat == resID || matchGlob(idPat, resID) {
				return true
			}
		}
	}
	return false
}

// ScopeResource is the in-scope set for one SpiceDB resource type.
// IDs is the explicit set; Patterns describes bounded matchers
// against resource attributes. Source records provenance.
type ScopeResource struct {
	ResourceType string `json:"resourceType"`
	// IDs are explicit ids, or globs when read from Disallow (e.g. "ENG-*").
	IDs []string `json:"ids,omitempty"`
	// Patterns match resource attributes instead of ids; empty means id-only.
	Patterns []ResourcePattern `json:"patterns,omitempty"`
	// Source records who put this entry here (default binding, approval, …).
	Source Source `json:"source,omitempty"`
}

// ResourcePattern is a bounded predicate over resource attributes.
// Values are globs (filepath.Match). No nested expressions; if richer
// logic is needed, fall through to an ArgConstraint.CEL.
type ResourcePattern struct {
	Attrs map[string]string `json:"attrs,omitempty"`
}

// ScopeTools restricts which tool names may be invoked in this
// session. Allow defaults to "any envelope-permitted" when empty;
// Deny always wins.
type ScopeTools struct {
	// Allow lists invocable tool names; empty means any the envelope permits.
	Allow []string `json:"allow,omitempty"`
	// Deny always wins over Allow, whatever else matched.
	Deny []string `json:"deny,omitempty"`
}

// IsZero reports whether ScopeTools carries no constraints, enabling
// the "omitzero" JSON tag to suppress the field when empty.
func (t ScopeTools) IsZero() bool {
	return len(t.Allow) == 0 && len(t.Deny) == 0
}

// ArgConstraint restricts the args a specific tool may receive.
// Equals/Forbid are flat key->value matchers; CEL is a bounded
// expression evaluated against `args`. All applicable constraints
// must hold; a constraint with only one field set leaves the others
// unconstrained.
type ArgConstraint struct {
	// Tool this constraint applies to; exact name or glob.
	Tool string `json:"tool"`
	// Equals requires each named arg to hold exactly this value.
	Equals map[string]string `json:"equals,omitempty"`
	// Forbid rejects the call when a named arg holds this value.
	Forbid map[string]string `json:"forbid,omitempty"`
	// CEL is a bounded expression over `args`; empty means no expression check.
	CEL string `json:"cel,omitempty"`
}

// ResourceRef is a typed SpiceDB resource pointer used in deltas and
// hard-deny relations.
type ResourceRef struct {
	ResourceType string `json:"resourceType"`
	ID           string `json:"id"`
}

func (r ResourceRef) String() string {
	return fmt.Sprintf("%s:%s", r.ResourceType, r.ID)
}

// IsGlobID reports whether id contains glob metacharacters (filepath.Match).
// Glob IDs (e.g. "ENG-*") and concrete IDs alike are enforced at Layer 2
// (Scope.Disallow, matched at dispatch by ResourceDisallowed) — the sole
// hard-deny surface after Layer-3 SpiceDB disallow retirement.
func IsGlobID(id string) bool {
	return strings.ContainsAny(id, "*?[")
}
