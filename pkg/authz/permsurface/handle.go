// Package permsurface enumerates the permission classes an agent's tool
// envelope can reach.
//
// It is a PURE VALUE PACKAGE: it imports pkg/authz and stdlib only, never a
// tool package. Callers project their tools into []Candidate. This keeps the
// enumeration testable without tool fakes and prevents an import cycle.
//
// The governing invariant, which every consumer depends on: the surface is
// exactly the set of permission classes the dispatcher will check — never a
// superset, never a subset.
package permsurface

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	permPrefix = "perm:"
	toolPrefix = "tool:"
)

// Component charsets. Each is already this repo's convention or the LLM
// provider's, and critically NONE of them admits ":" — which is what makes
// the wire format unambiguous. The point of validating rather than assuming
// is that these inputs are partly attacker-influenced (upstream MCP tool
// names), and nothing else in the codebase checks them.
//
// ASCII-only is load-bearing beyond ambiguity: it makes homoglyph spoofing
// and newline injection into an approval card structurally impossible.
var (
	// permissionRe mirrors BoundEntityType.Permission's kubebuilder pattern.
	permissionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// resourceTypeRe mirrors BoundEntityType.ResourceType's pattern; the
	// optional "/"-separated segments are SpiceDB namespaced object types.
	resourceTypeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`)
	// toolNameRe is the LLM provider convention for tool names.
	toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

// Handle is the durable, opaque reference to a permission class. It is the
// ONLY form that persists: phase ceilings, approval records, audit entries.
//
// SCOPE OF "UNFORGEABLE": the unexported field constrains OUR code, not the
// agent. A Handle in a plan is UnmarshalJSON'd out of the agent's own
// update_plan arguments, so the agent can mint any well-formed handle it
// likes. What actually constrains the agent is surface RESOLUTION (an
// unresolvable handle is inert) plus the upstream scope / tool_call_authz
// gates. This buys internal consistency — no code path can concatenate a
// handle into existence — not an agent-facing control.
//
// Handle is comparable: `==` is the correct way to test handle equality.
type Handle struct{ s string }

// NewPermHandle mints the handle for a (permission, resourceType) pair — the
// class of every tool whose PermissionCheck names that pair.
func NewPermHandle(permission, resourceType string) (Handle, error) {
	if !permissionRe.MatchString(permission) {
		return Handle{}, fmt.Errorf("permsurface: invalid permission %q: must match %s", permission, permissionRe)
	}
	if !resourceTypeRe.MatchString(resourceType) {
		return Handle{}, fmt.Errorf("permsurface: invalid resourceType %q: must match %s", resourceType, resourceTypeRe)
	}
	return Handle{s: permPrefix + permission + ":" + resourceType}, nil
}

// NewToolHandle mints the handle for a tool that carries no PermissionCheck
// (e.g. an external tool with no SpiceDB resource).
func NewToolHandle(toolName string) (Handle, error) {
	if !toolNameRe.MatchString(toolName) {
		return Handle{}, fmt.Errorf("permsurface: invalid tool name %q: must match %s", toolName, toolNameRe)
	}
	return Handle{s: toolPrefix + toolName}, nil
}

// ParseHandle re-derives a Handle from its wire form, re-running full
// component validation. It never sanitizes: a malformed handle is an error,
// so a persisted ceiling cannot smuggle an unvalidated value back in.
func ParseHandle(s string) (Handle, error) {
	switch {
	case strings.HasPrefix(s, permPrefix):
		permission, resourceType, ok := strings.Cut(strings.TrimPrefix(s, permPrefix), ":")
		if !ok {
			return Handle{}, fmt.Errorf("permsurface: malformed handle %q: want perm:<permission>:<resourceType>", s)
		}
		return NewPermHandle(permission, resourceType)
	case strings.HasPrefix(s, toolPrefix):
		return NewToolHandle(strings.TrimPrefix(s, toolPrefix))
	default:
		return Handle{}, fmt.Errorf("permsurface: unknown handle kind in %q: want a %q or %q prefix", s, permPrefix, toolPrefix)
	}
}

// String returns the wire form. The zero Handle stringifies to "".
func (h Handle) String() string { return h.s }

// ResourceType returns the SpiceDB resource type a perm handle names, or "" for
// a tool handle and the zero Handle.
//
// It exists so the plan gate can ask which resource types a phase's ceiling
// touches — the question behind "this phase acts on a git_repo but named no
// repository". The fact is already in the wire form; an accessor here keeps
// every consumer from re-splitting it, and a consumer that splits it slightly
// differently is exactly how an unvalidated value gets back in.
//
// Safe to read without re-validating: a Handle can only be built through
// NewPermHandle / NewToolHandle / ParseHandle, all of which validate the
// components first.
func (h Handle) ResourceType() string {
	if !strings.HasPrefix(h.s, permPrefix) {
		return ""
	}
	_, resourceType, ok := strings.Cut(strings.TrimPrefix(h.s, permPrefix), ":")
	if !ok {
		return ""
	}
	return resourceType
}

// IsZero reports whether h is the zero (invalid/absent) Handle.
func (h Handle) IsZero() bool { return h.s == "" }

// MarshalJSON emits the wire form. The unexported field means the default
// marshaller would emit "{}", so this is required, not cosmetic.
func (h Handle) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.s)
}

// UnmarshalJSON validates on the way in. Decoding is the boundary a persisted
// ceiling crosses back into the process, so it re-runs full validation rather
// than trusting stored bytes.
func (h *Handle) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("permsurface: handle must be a JSON string: %w", err)
	}
	parsed, err := ParseHandle(s)
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

// Handles returns each descriptor's handle in wire form, sorted.
//
// Three places needed this list — the runner's prompt (the vocabulary a plan
// may use), the notice returned when a handle is dropped, and the gate's own
// reporting — and each was building it inline. Sorted here because two of the
// three render it to an agent or a human, and a list whose order moves between
// turns reads as a different list.
func Handles(ds []Descriptor) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Handle.String())
	}
	sort.Strings(out)
	return out
}

// Permission is the permission half of a perm: handle, or "" for a tool:
// handle that names none.
//
// The sibling of ResourceType, and added for the same reason: the fact is
// already in the wire form, and a consumer that re-splits it slightly
// differently is how an unvalidated value gets back in.
//
// Its caller is slot binding. A slot grant is written per (instance,
// permission), so an approval that adds `read` to a type whose slot was
// declared for `push` must bind read — binding the slot's declared permission
// instead writes a grant the approved call can never spend, and the call
// escalates again on the very next attempt.
//
// Safe to read without re-validating: a Handle can only be built through
// NewPermHandle / NewToolHandle / ParseHandle, all of which validate first.
func (h Handle) Permission() string {
	if !strings.HasPrefix(h.s, permPrefix) {
		return ""
	}
	permission, _, ok := strings.Cut(strings.TrimPrefix(h.s, permPrefix), ":")
	if !ok {
		return ""
	}
	return permission
}
