// Package sandboxkinds is the pluggability seam for sandbox backends: the
// substrate a SpiceboxSession's tools actually execute in. The built-in "pod"
// kind runs a corev1.Pod; other kinds may run a peer CRD's resource, or
// something outside the cluster entirely.
//
// The seam is lifecycle verbs over an OPAQUE handle, deliberately not a pod
// constructor. Pod construction remains a helper (pkg/platform/podspec) that
// pod-shaped kinds import; it is not part of this interface, because a backend
// that is not Kubernetes could not implement it.
package sandboxkinds

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
)

// Kind is one sandbox backend. Implementations are stateless singletons
// registered at init(); everything stateful lives on Runtime.
type Kind interface {
	// Name is the registry key, matching SpiceboxClass.spec.sandbox.kind.
	Name() string

	// Supports reports whether the backend can satisfy a class requirement.
	// Static by necessity: SpiceboxClass validation runs before any backend
	// client exists, so this must be answerable without a Runtime.
	Supports(f Feature) bool

	// WorkspaceDomain names the storage world this backend's sandboxes can
	// share a workspace within. Two bundles of one AgentSession can share a
	// workspace only if their resolved kinds report the SAME non-empty domain.
	// "" means the backend cannot share a workspace at all.
	//
	// Supports(FeatureSharedWorkspace) is not sufficient on its own: two kinds
	// can each support sharing and still be unable to share with each other.
	WorkspaceDomain() string

	// ValidateClass rejects a class this backend cannot run. Joins the
	// existing validation chain in the spiceboxclass controller.
	ValidateClass(spec v1alpha1.SpiceboxClassSpec) error

	// NewRuntime constructs the backend's client-holding runtime. Returns an
	// error when the backend is unavailable (e.g. a peer CRD is not
	// installed), which is how bring-your-own backends fail closed.
	//
	// Callers MUST declare the receiving variable as Runtime, not as a
	// concrete pointer type: assigning a typed-nil pointer into an interface
	// produces a non-nil interface that panics on first use.
	NewRuntime(deps Deps) (Runtime, error)
}

// Runtime holds a backend's clients and performs its lifecycle operations.
// Constructed once per kind at startup, not per reconcile.
type Runtime interface {
	// Ensure reconciles the sandbox toward existence and returns its handle.
	//
	// MUST be idempotent, and MUST return the handle of an existing sandbox it
	// discovers rather than creating a second one. The status write that
	// persists the handle can fail after Ensure succeeds, so the next reconcile
	// re-enters with no handle in hand and must converge on the same sandbox.
	Ensure(ctx context.Context, req EnsureRequest) (Handle, error)

	// Status maps backend state onto AP's phase vocabulary. A sandbox that no
	// longer exists is PhaseGone, not an error.
	Status(ctx context.Context, h Handle) (Status, error)

	// Teardown removes the sandbox. Idempotent; an already-gone sandbox is
	// success, not an error.
	Teardown(ctx context.Context, h Handle) error

	// Executor returns an exec transport already BOUND to h. Callers pass no
	// sandbox identity in the Request — the binding is the handle.
	//
	// The returned executor satisfies exec.StreamingExecutor only when the
	// backend can actually stream, so the existing type-assertion
	// degradation at the call sites keeps working.
	Executor(h Handle) (exec.Executor, error)

	// Watches contributes the sources a controller must watch to observe
	// backend state changes. A backend with nothing cluster-local to watch
	// returns nil and relies on periodic requeue.
	Watches() []Watch
}

// Handle identifies one live sandbox. Opaque to AP: only the owning kind
// interprets Ref. Persisted to SpiceboxSession.status.sandbox, so it must
// survive an operator restart and a controller handoff.
type Handle struct {
	// Kind is the sandbox kind that owns Ref. Carried so a class that flips
	// kinds mid-life cannot have its old handle read by the new backend.
	Kind string
	// Ref is the backend's own identifier. The pod kind uses
	// "<namespace>/<podName>"; another backend may use a bare ID.
	Ref string
	// Prewarmed reports whether this sandbox was adopted from a pool of
	// already-running capacity rather than created on demand.
	//
	// LOAD-BEARING, not decoration. A pre-warming backend may resolve Ref
	// differently for an adopted sandbox than a cold one — agent-sandbox does,
	// since an adopted Sandbox's name is pool-generated and Ref names the claim
	// instead. Status, Teardown and Executor resolve from the handle alone, so
	// this flag MUST be persisted and rehydrated alongside Ref (see
	// v1alpha1.SandboxHandle); dropping it sends the verb down the wrong
	// resolution path rather than merely losing observability. False is the
	// safe default — it selects the cold path.
	Prewarmed bool
}

// HandleFromStatus rehydrates a Handle from the form persisted on
// SpiceboxSession.status.sandbox. Every consumer that reads a stored handle
// MUST go through this rather than composing a Handle literal: a new field
// added to Handle is then carried at every site by construction, instead of
// being remembered at three call sites and silently dropped at the fourth.
// Returns the zero Handle for a nil input, which fails closed at ResolveHandle.
func HandleFromStatus(h *v1alpha1.SandboxHandle) Handle {
	if h == nil {
		return Handle{}
	}
	return Handle{Kind: h.Kind, Ref: h.Ref, Prewarmed: h.Prewarmed}
}

// ToStatus renders h in its CRD-serializable form, for the controller that
// persists it. The inverse of HandleFromStatus, and here for the same reason.
func (h Handle) ToStatus() *v1alpha1.SandboxHandle {
	return &v1alpha1.SandboxHandle{Kind: h.Kind, Ref: h.Ref, Prewarmed: h.Prewarmed}
}

// Status is a backend's state in AP's vocabulary. Kinds map their own
// conditions onto this; consumers never see backend-native state.
type Status struct {
	Phase Phase
	// Reason is a condition reason, surfaced on the SpiceboxSession.
	Reason string
	// Message is human-readable and surfaced to the user.
	Message string

	// RequiresPolling reports that NONE of this backend's own Watches() will
	// produce an event that ends the state being reported, so the consumer
	// must re-ask on a timer or the session stalls until the manager's
	// ~10-hour resync.
	//
	// BACKEND KNOWLEDGE, not scheduling policy: only the backend knows which of
	// its own states its own watches cover. The pod kind's Pending IS covered
	// (Owns(&corev1.Pod{}) fires when the pod goes Ready), so it never sets
	// this; agent-sandbox's "adopted, waiting for session labels" Pending is
	// not — see that backend's Watches for why.
	//
	// A BOOL, not a duration: the fact is the backend's to state, but the
	// interval is cluster-wide reconcile load and must stay tunable in ONE
	// place. A bool also avoids "does zero mean never or immediately".
	//
	// SET IT AS NARROWLY AS POSSIBLE: per STATE, never per phase. True for
	// every Pending re-reconciles every starting sandbox in the cluster on a
	// timer — a steady-state load increase, not a backstop.
	RequiresPolling bool
}

// Phase is the backend-independent lifecycle state of a sandbox.
type Phase string

const (
	// PhasePending means creating, scheduling, or not yet accepting exec.
	PhasePending Phase = "Pending"
	// PhaseReady means the sandbox accepts exec.
	PhaseReady Phase = "Ready"
	// PhaseFailed is terminal; Reason/Message say why.
	PhaseFailed Phase = "Failed"
	// PhaseGone means the sandbox no longer exists — deleted, evicted, or
	// expired. Not an error: Teardown and a completed session both land here.
	PhaseGone Phase = "Gone"
)

// Feature is a capability question asked at CLASS-VALIDATION time, where no
// Runtime exists yet. Runtime verbs are optional interfaces instead
// (Snapshotter, Suspender, FileTransferer), so each fact has exactly one home
// and there is no boolean to keep in sync with a nil return.
type Feature string

const (
	// FeatureSharedWorkspace: one workspace shared across a session's bundles.
	FeatureSharedWorkspace Feature = "shared-workspace"
	// FeatureConfigMapMounts: SpiceboxClass.spec.mounts.
	FeatureConfigMapMounts Feature = "configmap-mounts"
	// FeatureToolchainOverlay: SpiceboxClass.spec.toolchains.
	FeatureToolchainOverlay Feature = "toolchain-overlay"
	// FeatureHostEgressAllowlist: hostname-level egress actually ENFORCED,
	// as opposed to merely recorded on status.
	FeatureHostEgressAllowlist Feature = "host-egress-allowlist"
	// FeatureUnpackMounts: a SpiceboxMount whose Format is not raw, which
	// needs an initContainer to expand archive content before the app
	// container starts. Distinct from FeatureConfigMapMounts: a backend can
	// mount a ConfigMap without being able to run an initContainer.
	FeatureUnpackMounts Feature = "unpack-mounts"
)

// ClassValidationFeatures returns every Feature. It exists so a test can
// assert the set stays closed: a Feature that is not a class-validation
// question belongs as an optional interface instead.
func ClassValidationFeatures() []Feature {
	return []Feature{
		FeatureSharedWorkspace,
		FeatureConfigMapMounts,
		FeatureToolchainOverlay,
		FeatureHostEgressAllowlist,
		FeatureUnpackMounts,
	}
}

// Deps are the dependencies a Runtime is constructed with. Optional fields are
// nil-safe; a kind that does not need one ignores it. Dependencies flow IN to
// the kind — the same direction as channelkinds.Deps.
type Deps struct {
	// Client is the controller-runtime client, for kinds whose backend is a
	// Kubernetes object. nil for backends outside the cluster.
	Client client.Client

	// ExecFor returns an exec transport bound to one sandbox. Held as a binder
	// rather than a ready executor because a Runtime serves many sandboxes and
	// each call must reach exactly one — the transport, not the caller, decides
	// how a sandbox is addressed. nil for backends that do not exec over this
	// mechanism.
	ExecFor func(namespace, pod, container string) exec.Executor

	// Logger is used for the log half of the no-silent-errors rule.
	Logger logr.Logger

	// MonitoringPublish fans a cluster-operator-facing warning out to every
	// role=monitoring Channel, via channelevents.PublishMonitoring, for a
	// degradation an operator must act on but no session participant can.
	// Deliberately NOT the session-participant notice path
	// (pkg/channels/channelinteractions), whose audiences are the wrong readers
	// for a cluster misconfiguration.
	//
	// OPTIONAL and nil-safe — an unconfigured bus leaves a genuine nil. Every
	// emit site MUST nil-guard and MUST still work without it: monitoring is
	// how an operator learns of a degradation, never how one is decided.
	MonitoringPublish channelevents.PublishFunc
}

// EnsureRequest is the kind-agnostic intent: everything a backend needs to
// bring a sandbox into existence. Carries AP domain types only — a backend
// outside the cluster must be able to read all of it.
type EnsureRequest struct {
	// Session is the SpiceboxSession the sandbox belongs to.
	Session *v1alpha1.SpiceboxSession
	// Class is the already-resolved, frozen class spec.
	Class v1alpha1.SpiceboxClassSpec
	// Config is the resolved spec.sandbox.config passthrough. Opaque to AP;
	// each kind parses its own. nil when unset.
	Config *apiextensionsv1.JSON
}

// Watch is one source a controller must watch to observe backend state
// changes, contributed by the kind rather than hard-coded in the controller.
type Watch struct {
	// Object is the owned object type to watch (the pod kind contributes
	// &corev1.Pod{}). Mapped back to the owning SpiceboxSession by the
	// controller's existing owner-reference handling.
	Object client.Object
}

// Snapshotter is an optional interface a Runtime may implement when its
// backend can capture and restore filesystem state. Consumers type-assert;
// there is deliberately no parallel Feature flag.
type Snapshotter interface {
	Snapshot(ctx context.Context, h Handle) (SnapshotRef, error)
	Restore(ctx context.Context, h Handle, ref SnapshotRef) error
}

// SnapshotRef identifies a snapshot to the backend that produced it. Opaque
// to AP, like Handle.
type SnapshotRef struct {
	Kind string
	Ref  string
}

// Suspender is an optional interface a Runtime may implement when its backend
// can release compute without destroying state.
type Suspender interface {
	Suspend(ctx context.Context, h Handle) error
	Resume(ctx context.Context, h Handle) error
}

// FileTransferer is an optional interface a Runtime may implement when its
// backend has a native file API. Implementing it lets artifact hydrate/harvest
// skip streaming a tar through exec.
//
// The tar path an implementation replaces already behaves as follows, so an
// implementation MUST match it or artifacts will land differently depending on
// which backend a session happens to run:
//
//   - PutFile creates missing parent directories with mode 0755, and the file
//     itself with mode 0644.
//   - Regular files only, in both directions. GetFile does not return a
//     directory, symlink, device or other special entry, and PutFile never
//     creates one.
//   - Neither call resolves a symlink it encounters in the path it was given.
//     A backend whose native API follows symlinks server-side MUST refuse the
//     path instead: following one is a way out of the sandbox's own
//     filesystem, and the callers do not re-check what a path resolved to.
//   - GetFile MAY return more bytes than the caller wants; it carries no size
//     bound of its own. Callers bound the result themselves.
//
// Both calls MUST honour ctx: the caller's deadline is the only thing bounding
// them, and ToolCall reconciliation is serialized, so a call that ignores
// cancellation stalls every ToolCall in the cluster.
type FileTransferer interface {
	PutFile(ctx context.Context, h Handle, path string, data []byte) error
	GetFile(ctx context.Context, h Handle, path string) ([]byte, error)
}

// Prewarmer is an optional interface a Runtime may implement when its backend
// can keep sandboxes ready before any session asks for one. Consumers
// type-assert; there is deliberately no parallel Feature, so each fact has one
// home.
//
// Adoption is NOT part of this interface. A backend that pre-warms adopts
// inside its own Ensure and returns the adopted sandbox's handle, so the
// session path needs no knowledge of pooling.
type Prewarmer interface {
	// ReconcilePool brings the backend's pre-warmed capacity for one class in
	// line with req.Replicas. MUST be idempotent — it runs on every class
	// reconcile. Replicas == 0 means tear the pool down.
	//
	// Returns an error wrapping ErrPrewarmingUnavailable when the backend
	// implements Prewarmer but cannot currently honor a non-zero request — a
	// peer CRD that pre-warming alone depends on not being installed, say. The
	// type assertion consumers make at validation time answers "does this
	// backend implement pre-warming", never "can it right now", so this is the
	// only place the fact is knowable, and the sentinel is what lets the
	// spiceboxclass controller stamp a validation condition instead of
	// requeuing on what looks like a transient failure.
	ReconcilePool(ctx context.Context, req PoolRequest) error

	// SweepOrphanedPools reclaims pre-warmed capacity ReconcilePool no longer
	// iterates toward: a namespace removed from the desired set, or the set
	// going empty. ReconcilePool only acts on the namespaces the CURRENT
	// request names, so anything dropped from it would run — and bill —
	// forever with nothing pointing at it.
	//
	// Empty desiredNamespaces means "reclaim every pool ever created for
	// className, in every namespace". MUST be idempotent and safe to call on
	// every class reconcile; finding nothing to delete is the common case.
	SweepOrphanedPools(ctx context.Context, className string, classUID types.UID, desiredNamespaces []string) error
}

// ErrPrewarmingUnavailable is the sentinel a Prewarmer.ReconcilePool wraps
// when its backend implements Prewarmer but cannot currently satisfy a
// non-zero request. It lives here rather than in a backend so every
// implementation reports the fact identically and a consumer testing
// errors.Is never needs to know which backend it is talking to.
var ErrPrewarmingUnavailable = errors.New("sandboxkinds: pre-warming is not currently available for this backend")

// PoolRequest is the kind-agnostic intent for pre-warmed capacity. Carries AP
// domain types only, so a backend outside the cluster could implement it.
type PoolRequest struct {
	// ClassName and Namespace identify the SpiceboxClass the pool serves.
	ClassName string
	Namespace string
	// ClassUID is the owning SpiceboxClass's UID, set by the spiceboxclass
	// controller. A backend creating cluster objects for the pool MUST use it
	// as their ownerReference so deleting the class garbage-collects them
	// rather than leaving them running (and billing) unreferenced. Empty is a
	// fail-closed error, never a silent unowned create.
	ClassUID types.UID
	// Class is the frozen class spec the pooled sandboxes are built from.
	Class v1alpha1.SpiceboxClassSpec
	// Toolchains is the class's RESOLVED toolchain mounts, as resolved by the
	// spiceboxclass controller at reconcile time. Carried explicitly rather than
	// resolved by the backend: resolution reads cluster-scoped SpiceboxToolchain
	// CRs, and PoolRequest deliberately carries AP domain types only so a
	// backend outside the cluster could implement Prewarmer.
	//
	// Empty means the class declares no toolchains — NOT that resolution failed.
	// A failed resolution never reaches a backend; the controller skips pooling
	// entirely (see the spiceboxclass controller).
	Toolchains []v1alpha1.ToolchainMount
	// Replicas is how many warm sandboxes to keep. 0 tears the pool down.
	Replicas int32
}
