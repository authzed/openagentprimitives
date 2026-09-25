// Package pinning defines the pluggable dependency-pinning abstraction: every
// kind of swappable dependency an agent opts into (skills, sidecar images,
// MCP servers, CLI toolkits) has a user-facing ref with a measurable pin
// strength, a frozen identity it can be resolved to, and a way to detect
// drift between the recorded baseline and live state. Per-kind semantics live
// in pkg/authz/pinning/kinds/<name>/ and register via pkg/authz/pinning/registry.
package pinning

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Strength classifies how tightly a dependency ref is pinned.
type Strength string

const (
	StrengthFrozen   Strength = "frozen"   // immutable identity: sha, digest, manifest hash
	StrengthNamed    Strength = "named"    // movable name: tag, branch, version range
	StrengthUnpinned Strength = "unpinned" // rolling: no ref at all
)

// Level orders strengths for floor comparisons: frozen(2) > named(1) >
// unpinned(0). Unknown values order with unpinned.
func (s Strength) Level() int {
	switch s {
	case StrengthFrozen:
		return 2
	case StrengthNamed:
		return 1
	default:
		return 0
	}
}

// Ref is a parsed, classified dependency reference.
type Ref struct {
	Kind     string   // registry kind name (e.g. "skill")
	Spec     string   // the user-facing ref string, normalized
	Strength Strength // syntactic classification of Spec
}

// Frozen is the resolved immutable identity of a dependency. ObservedAt is
// stamped by the recording controller (not by Resolve) so kinds stay
// clock-free and deterministic under test.
type Frozen struct {
	Digest     string            // git sha | sha256:… | manifest hash | binary hash
	Version    string            // human-readable: tag | serverInfo.version | probe output
	ObservedAt metav1.Time       // when the recording controller observed it
	Details    map[string]string // kind-specific extras (toolCount, registry, …)
}

// DriftReport is the outcome of comparing live state against a recorded
// baseline. Summary is human-readable and feeds tool warnings, approval
// prompts, and monitoring-channel alerts.
type DriftReport struct {
	Drifted bool
	Old     Frozen
	New     Frozen
	Summary string
}

// Kind is one pluggable dependency kind. Implementations live under
// pkg/authz/pinning/kinds/<name>/ and register via pkg/authz/pinning/registry.
type Kind interface {
	// Name returns the registry kind name (e.g. "skill").
	Name() string
	// ParseRef classifies the user-facing ref string. It is pure (no I/O).
	ParseRef(spec string) (Ref, error)
	// Resolve produces the frozen identity for a ref. May perform I/O
	// (registry lookups, manifest fetches) depending on the kind.
	Resolve(ctx context.Context, r Ref) (Frozen, error)
	// Verify compares live state against a recorded baseline.
	Verify(ctx context.Context, r Ref, baseline Frozen) (DriftReport, error)
}
