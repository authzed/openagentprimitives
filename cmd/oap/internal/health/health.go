// Package health is oap's registry of cluster components and their health checks.
//
// `oap check` iterates this registry instead of hardcoding a single deployment:
// each Component defines how to verify itself (Check) and MAY define how to fix
// itself (Repairer), which `oap check --repair` invokes on failures. A new
// component is added by registering it here — the same interface+registry shape
// the rest of the project uses for pluggable aspects — so the check command and
// every consumer stay untouched.
//
// This exists because `oap check` previously verified only spicebox-operator and
// reported "all checks passed" while authzd and channelsd were CrashLoopBackOff:
// the missing components were simply never looked at.
package health

import (
	"context"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// namespace is where every first-party component runs.
const namespace = "agentprimitives-system"

// Status is a component's health verdict.
type Status int

const (
	// OK — healthy.
	OK Status = iota
	// Optional — not healthy, but non-fatal: the component is not required for
	// the core stack to function (e.g. graphiti without OPENAI_API_KEY). `oap
	// check` warns but does not fail.
	Optional
	// Failed — unhealthy; `oap check` fails.
	Failed
)

// Result is the outcome of a component's health check.
type Result struct {
	Status Status
	// Detail is a short human-readable explanation, e.g. "2/2 ready" or
	// "0/1 ready (CrashLoopBackOff)".
	Detail string
	// NotFound is true iff the component's underlying workload object does not
	// exist at all (its Get returned an IsNotFound error), as opposed to
	// existing but being unready. Status is still set (Failed for required
	// components) so `oap check` behavior is unchanged; NotFound lets a consumer
	// that runs a trimmed profile (e.g. `oap desktop`'s minimal local stack)
	// SKIP a component the profile simply never deploys, rather than alarming.
	NotFound bool
}

// Component is a cluster component that `oap check` verifies. Each component
// owns its own health check; implement Repairer as well if it can fix itself.
type Component interface {
	Name() string
	Check(ctx context.Context, b *kube.Bundle) Result
}

// Repairer is the optional self-repair capability. `oap check --repair` calls
// Repair on any component whose Check returned Failed and that implements it.
// Repair must be idempotent and safe to run against an already-failing
// component (it only ever runs on Failed components).
type Repairer interface {
	Repair(ctx context.Context, b *kube.Bundle) error
}

var registry []Component

// Register adds a component to the registry. Call it from an init() so the set
// is assembled at import time.
func Register(c Component) { registry = append(registry, c) }

// All returns the registered components in registration order (a copy, so
// callers cannot mutate the registry).
func All() []Component { return append([]Component(nil), registry...) }
