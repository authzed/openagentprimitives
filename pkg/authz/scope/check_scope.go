package scope

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/authzed/openagentprimitives/pkg/x/celbudget"
	"github.com/google/cel-go/cel"
)

// Result is the outcome of CheckScope. OK=true means the tool call
// satisfies every relevant constraint in the Scope. Reason is a
// machine-parseable category; Message is human-readable for logs.
type Result struct {
	// OK is the verdict; false always carries a Reason.
	OK bool `json:"ok"`
	// Reason is one of the Reason* constants below; empty when OK.
	Reason string `json:"reason,omitempty"`
	// Message is human-readable detail for logs; never machine-parsed.
	Message string `json:"message,omitempty"`
}

// Reasons for denial. Kept as string constants so callers can switch
// on them; not an enum type to keep the surface small.
const (
	ReasonToolDeniedByPolicy    = "tool_denied_by_policy"
	ReasonToolNotInAllowList    = "tool_not_in_allow_list"
	ReasonArgConstraintViolated = "arg_constraint_violated"
	// ReasonResourceNotInScope is returned only by CheckScopeWithRefs, which
	// nothing calls — a consumer switching on Reason will never observe it in
	// production today. See the NOT WIRED note on CheckScopeWithRefs.
	ReasonResourceNotInScope = "resource_not_in_scope"
)

// CheckScope evaluates `scope` against a single tool dispatch. Runs
// in `CheckToolCall` before any SpiceDB Check. Returns OK=true if the
// dispatch is allowed by the policy doc (Layer 2), false otherwise.
// `args` is the raw JSON-encoded tool arguments; arg-constraint and
// resource-in-scope checks parse it as needed.
func CheckScope(s Scope, toolName string, args json.RawMessage) Result {
	// Deny list wins over allow.
	for _, pat := range s.Tools.Deny {
		if matchGlob(pat, toolName) {
			return Result{
				OK:      false,
				Reason:  ReasonToolDeniedByPolicy,
				Message: fmt.Sprintf("tool %s denied by session policy (matched %q)", toolName, pat),
			}
		}
	}

	// Empty allow list = "any envelope-permitted tool" — pass through.
	if len(s.Tools.Allow) > 0 {
		hit := false
		for _, pat := range s.Tools.Allow {
			if matchGlob(pat, toolName) {
				hit = true
				break
			}
		}
		if !hit {
			return Result{
				OK:      false,
				Reason:  ReasonToolNotInAllowList,
				Message: fmt.Sprintf("tool %s not in session allow-list", toolName),
			}
		}
	}

	if r := checkArgConstraints(s.ArgConstraints, toolName, args); !r.OK {
		return r
	}

	return Result{OK: true}
}

// checkArgConstraints evaluates Equals + Forbid against args. CEL
// evaluation lands in A5.
func checkArgConstraints(cs []ArgConstraint, toolName string, args json.RawMessage) Result {
	parsed := map[string]any{}
	if len(args) > 0 {
		// Fail closed on malformed args: evaluating a Forbid/CEL constraint
		// against an empty map would silently PASS. Both production callers
		// pre-validate (the Scope hook denies on parse error before calling;
		// CheckToolCall re-marshals an already-parsed map), so this only
		// fires on a future/defensive path — but the conservative direction
		// at the authz boundary is to deny, never silently pass.
		if err := json.Unmarshal(args, &parsed); err != nil {
			return Result{
				OK:      false,
				Reason:  ReasonArgConstraintViolated,
				Message: fmt.Sprintf("tool %s: args unparseable; cannot evaluate scope constraints (fail-closed): %v", toolName, err),
			}
		}
	}
	for _, c := range cs {
		if c.Tool != toolName {
			continue
		}
		for k, v := range c.Equals {
			got, ok := parsed[k]
			if !ok || fmt.Sprintf("%v", got) != v {
				return Result{
					OK:      false,
					Reason:  ReasonArgConstraintViolated,
					Message: fmt.Sprintf("arg %q must equal %q (got %v)", k, v, got),
				}
			}
		}
		for k, v := range c.Forbid {
			got, ok := parsed[k]
			if ok && fmt.Sprintf("%v", got) == v {
				return Result{
					OK:      false,
					Reason:  ReasonArgConstraintViolated,
					Message: fmt.Sprintf("arg %q must not equal %q", k, v),
				}
			}
		}
		if c.CEL != "" {
			ok, err := evalCEL(c.CEL, parsed)
			if err != nil {
				return Result{
					OK:      false,
					Reason:  ReasonArgConstraintViolated,
					Message: fmt.Sprintf("CEL evaluation error on tool %s: %v", toolName, err),
				}
			}
			if !ok {
				return Result{
					OK:      false,
					Reason:  ReasonArgConstraintViolated,
					Message: fmt.Sprintf("CEL constraint failed for tool %s: %s", toolName, c.CEL),
				}
			}
		}
	}
	return Result{OK: true}
}

// evalCEL compiles + runs a CEL expression against the args map.
// Errors surface as non-nil; non-bool return surfaces as an error.
func evalCEL(expr string, args map[string]any) (bool, error) {
	env, err := cel.NewEnv(cel.Variable("args", cel.DynType))
	if err != nil {
		return false, err
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return false, iss.Err()
	}
	prg, err := env.Program(ast, celbudget.ProgramOptions()...)
	if err != nil {
		return false, err
	}
	out, _, err := prg.Eval(map[string]any{"args": args})
	if err != nil {
		return false, err
	}
	if b, ok := out.Value().(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("CEL expression %q must return bool, got %T", expr, out.Value())
}

// AttrsLookup resolves the SpiceDB resource attributes used by
// ResourcePattern matching. Returns nil if the resource's attrs
// aren't known (in which case patterns can't match it).
type AttrsLookup func(r ResourceRef) map[string]string

// CheckScopeWithRefs evaluates `scope` against a dispatch that
// references the given typed resources: CheckScope's three axes plus the
// resource-in-scope axis, narrowing PER TYPE. A type absent from
// s.Resources is not narrowed at all; once a type IS present, every ref of
// that type must match an explicit ID or a pattern or the dispatch is denied.
// `attrs` is optional; if nil, only explicit-ID matches succeed.
//
// NOT WIRED. Nothing in production calls this, so ReasonResourceNotInScope
// is unreachable and the resource axis of a session's Scope is not enforced
// at dispatch — CheckToolCall calls plain CheckScope (check_tool_call.go),
// the dispatch hook calls CheckScope + Scope.ResourceDisallowed
// (hooks/scope.go), and the only other production reader of
// ScopeResource.IDs is FillToolArgs (authz/autofill.go), which fills an
// unset arg and is advisory. Practical consequence: an approved narrowing
// of s.Resources is recorded and acked with a ScopeVersion bump while
// constraining nothing.
//
// This is infrastructure landed ahead of its consumer, not dead weight — the
// design spec defines resources as a positive whitelist and lists the
// resource axis among CheckScope's (see the dynamic-session-scope design
// doc). Wiring it is a live authz change that needs the seeding path and the
// e2e suite behind it, so do not improvise a call site: when one lands,
// delete this paragraph and the matching note on authz.Inputs.SessionScope,
// which are the two places that describe this state.
func CheckScopeWithRefs(s Scope, toolName string, args json.RawMessage, refs []ResourceRef, attrs AttrsLookup) Result {
	if r := CheckScope(s, toolName, args); !r.OK {
		return r
	}
	for _, ref := range refs {
		var matched *ScopeResource
		for i := range s.Resources {
			if s.Resources[i].ResourceType == ref.ResourceType {
				matched = &s.Resources[i]
				break
			}
		}
		if matched == nil {
			// Type not in scope.Resources → scope doesn't narrow this type.
			continue
		}
		// Explicit ID match.
		if containsString(matched.IDs, ref.ID) {
			continue
		}
		// Pattern match if attrs available.
		if attrs != nil {
			a := attrs(ref)
			if a != nil && matchAnyPattern(matched.Patterns, a) {
				continue
			}
		}
		return Result{
			OK:      false,
			Reason:  ReasonResourceNotInScope,
			Message: fmt.Sprintf("%s not in session scope for %s", ref.String(), matched.ResourceType),
		}
	}
	return Result{OK: true}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// matchAnyPattern reports whether attrs satisfies at least one pattern.
// An empty Attrs map on a pattern is NOT a wildcard — it matches nothing.
func matchAnyPattern(patterns []ResourcePattern, attrs map[string]string) bool {
	for _, p := range patterns {
		if len(p.Attrs) == 0 {
			continue
		}
		hit := true
		for k, v := range p.Attrs {
			got, ok := attrs[k]
			if !ok {
				hit = false
				break
			}
			if !matchGlob(v, got) {
				hit = false
				break
			}
		}
		if hit {
			return true
		}
	}
	return false
}

// matchGlob is filepath.Match with the convention that an invalid
// pattern is treated as no-match (rather than propagating the error).
// Glob errors come from malformed Allow/Deny entries — failing closed
// (Deny: no-match → ignored; Allow: no-match → denied if Allow non-empty)
// is the conservative direction.
func matchGlob(pattern, s string) bool {
	ok, err := filepath.Match(pattern, s)
	if err != nil {
		return false
	}
	return ok
}
