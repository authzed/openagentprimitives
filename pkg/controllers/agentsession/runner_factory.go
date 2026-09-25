package agentsession

import (
	"context"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// StartOpts carries per-call inputs the reconciler computes fresh on
// each Start — values that depend on cluster state at spawn time and so
// can't live on the (long-lived) factory struct. Today it carries the
// resolved sidecar toolbox snapshot; the production PodRunnerFactory
// uses it to compose sidecar containers, while the in-process e2e
// factory has no real Pod and ignores the sidecar fields.
type StartOpts struct {
	// ResolvedSidecars is the per-session sidecar-toolbox snapshot, one
	// entry per AgentClass.spec.sidecarToolboxes ref. Nil/empty when the
	// class declares no sidecars. The reconciler computes this in its
	// step-4d sidecar block (resolve CR + SpiceboxClass, allocate ports,
	// merge network policy) and passes it here.
	ResolvedSidecars []spiceboxv1alpha1.ResolvedSidecarToolbox
	// SidecarSecretName maps a toolbox ref to the per-session Secret name
	// the reconciler materialized for it. Used by PodRunnerFactory to wire
	// each sidecar container's envFrom. May be nil when there are no
	// sidecars.
	//
	// CONTRACT: the reconciler MUST create every named Secret BEFORE
	// calling Start — the sidecar containers envFrom them, and a Pod that
	// references a non-existent Secret hangs in ContainerCreating
	// indefinitely (the kubelet never surfaces a hard error). This mirrors
	// the MemoryTokenSecretName must-exist-before-Start ordering. The
	// reconciler's step-4d materializeSidecarSecret satisfies this; the
	// echo/no-credential path still writes an (empty) Secret so envFrom
	// always resolves.
	SidecarSecretName func(ref string) string

	// ResolvedContentGuardDetectors is the per-session content-guard
	// detector snapshot. Each entry has a reflected PodIP once the
	// detector pod is Ready; the reconciler gates runner-pod creation
	// until every PodIP is non-empty. PodRunnerFactory injects
	// CONTENTGUARD_DETECTOR_ENDPOINT=http://<PodIP>:<Port> into the
	// runner container for each entry.
	ResolvedContentGuardDetectors []spiceboxv1alpha1.ResolvedContentGuardDetector
}

// RunnerFactory creates the per-session runner. The production
// implementation (PodRunnerFactory) creates a Kubernetes Pod; the
// e2e harness (test/e2e.InProcessRunnerFactory) creates an in-process
// goroutine running runner.Loop with a scripted LLM provider.
//
// The interface is intentionally minimal: the AgentSession controller
// only needs to spawn, tear down, and read back the workload name for
// status reflection (ObservedName). Everything else (NATS subjects,
// memory state, SpiceDB writes) flows through the per-session config
// the factory constructs internally. Per-call inputs that depend on
// live cluster state (resolved sidecars) ride in StartOpts.
type RunnerFactory interface {
	// Start launches the runner for the given session. Returns once
	// the runner is reachable (NATS subscriptions live, etc.) — not
	// once it finishes. Idempotent: calling Start a second time for
	// the same session must be a no-op (PodRunnerFactory's Create
	// returns AlreadyExists; InProcessRunnerFactory's map check).
	Start(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, class *spiceboxv1alpha1.AgentClass, opts StartOpts) error

	// Stop tears down the runner. Production: deletes the Pod (or
	// relies on owner-ref GC). In-process: cancels the goroutine's
	// context and waits up to 5s for it to exit. Best-effort —
	// errors are logged but don't block AgentSession finalization.
	Stop(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) error

	// ObservedName returns the name of the workload backing this session,
	// for status reflection by the reconciler. Returns "" when the runner
	// has no addressable workload the reconciler should record — the
	// in-process e2e factory, whose placeholder Pod is a scheduling
	// convenience the session status deliberately does not surface.
	ObservedName(sess *spiceboxv1alpha1.AgentSession) string
}
