// pkg/platform/pipeline/doc.go

// Package pipeline is the generic session-lifecycle interceptor framework.
//
// Lifecycle points (Point) mark moments in an agent session. Hooks (Hook)
// attach to points, are registered with a code-owned order (Registry), and
// return decisions as data (Decision). An Executor runs the hooks for a point
// and applies their effects through a Host port: it owns ordering, Deny/Halt
// short-circuit, ApprovalAsk publish/await/timeout, notice/status delivery,
// audit, and fail-closed-on-panic.
//
// The package imports no domain packages. pkg/authz/hooks is one consumer;
// non-authz concerns (output secret scanning, budget gating) register hooks the
// same way.
package pipeline
