package cel

import (
	"fmt"
	"strings"
	"sync"

	celgo "github.com/google/cel-go/cel"
)

// Scope is a bitmask that controls which CEL environment a Helper is
// available in. A Helper marked ScopeToolspec|ScopeMCP shows up in both.
type Scope uint8

const (
	// ScopeToolspec is the SpiceboxToolspec env (variable `call`).
	ScopeToolspec Scope = 1 << iota
	// ScopeMCP is the MCPServer arg-constraint env (variable `args`).
	ScopeMCP
)

// Helper is one CEL function (or set of overloads) the registry knows
// about. It bundles the runtime binding (EnvOptions) with the
// human-facing docs the LLM authoring prompts splice into their system
// text — single source of truth so the generator and the runner never
// drift on what's available.
type Helper struct {
	// Name is the dotted CEL name as the user writes it (e.g.
	// "path.isUnder", "now"). Used for sorting/lookup; also expected
	// to appear inside Signature.
	Name string
	// Signature is the LLM-facing display form, e.g.
	// "path.isUnder(p string, prefix string) -> bool". Surface form
	// only — the actual type-check happens against EnvOptions.
	Signature string
	// Doc is a one-line description. Multi-line ok if needed; the
	// formatter indents continuations.
	Doc string
	// Example is an optional CEL snippet the LLM can pattern-match
	// against. Empty = omitted from docs.
	Example string
	// Scope is the bitmask of envs this helper is wired into.
	Scope Scope
	// EnvOptions are the cel-go options applied to NewEnv when this
	// helper's scope matches. Typically one cel.Function(...) or a
	// cel.Lib(...) wrapping multiple member overloads.
	EnvOptions []celgo.EnvOption
}

var (
	registryMu sync.RWMutex
	registry   []Helper
)

// Register adds h to the package-level helper registry. Called from
// init() of helper-implementation files (helpers.go, time.go). Append-
// only; later registrations are visible to all subsequent Env / MCPEnv
// constructions.
func Register(h Helper) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = append(registry, h)
}

// HelpersFor returns the helpers whose Scope intersects want. Ordering
// is registration order. Callers that need a stable display order
// should sort by Name themselves.
func HelpersFor(want Scope) []Helper {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var out []Helper
	for _, h := range registry {
		if h.Scope&want != 0 {
			out = append(out, h)
		}
	}
	return out
}

// envOptionsFor returns the flattened cel.EnvOption slice for every
// helper whose Scope intersects want. Used by Env / MCPEnv.
func envOptionsFor(want Scope) []celgo.EnvOption {
	hs := HelpersFor(want)
	var opts []celgo.EnvOption
	for _, h := range hs {
		opts = append(opts, h.EnvOptions...)
	}
	return opts
}

// HelperDocs formats the registry's helpers for inclusion in an LLM
// authoring prompt. Output shape (registration order, two-space indent
// for the signature line, optional one-line doc and one-line example):
//
//	name.foo(a, b) -> bool
//	  description text
//	  example: name.foo(call.x, "y")
//
// Returns "" when no helpers match the scope, so callers can splice
// the result unconditionally.
func HelperDocs(want Scope) string {
	hs := HelpersFor(want)
	if len(hs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hs {
		sig := h.Signature
		if sig == "" {
			sig = h.Name + "(...)"
		}
		fmt.Fprintf(&b, "  %s\n", sig)
		if h.Doc != "" {
			for _, line := range strings.Split(strings.TrimRight(h.Doc, "\n"), "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
		if h.Example != "" {
			fmt.Fprintf(&b, "    example: %s\n", h.Example)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
