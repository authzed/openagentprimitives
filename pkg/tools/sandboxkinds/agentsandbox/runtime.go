package agentsandbox

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// Runtime is the agent-sandbox backend's lifecycle implementation. It owns no
// state beyond its injected dependencies and one startup-resolved capability
// bit; a sandbox's identity travels in the Handle.
type Runtime struct {
	deps sandboxkinds.Deps

	// prewarmAvailable reports whether this cluster serves the
	// extensions.agents.x-k8s.io CRDs (SandboxClaim / SandboxWarmPool /
	// SandboxTemplate), which ship separately from the base Sandbox CRD.
	//
	// Resolved once at NewRuntime from the RESTMapper, never per Ensure:
	// addressing an unserved kind through the operator's cached client starts
	// an informer that can never sync, and doing so on the session hot path
	// would turn "this cluster does not pre-warm" into a per-session error
	// instead of an ordinary cold start.
	prewarmAvailable bool
}

// Ensure brings the session's sandbox into existence and returns its handle.
// Idempotent: an existing sandbox — adopted or cold — or a concurrent creator
// winning the race yields the same handle rather than a second sandbox.
func (r *Runtime) Ensure(ctx context.Context, req sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	if req.Session == nil {
		return sandboxkinds.Handle{}, fmt.Errorf("EnsureRequest.Session is required")
	}
	ns := req.Session.Namespace
	// Named by the same helper the pod kind uses, so a session's sandbox has
	// one predictable name regardless of which backend renders it. The
	// SandboxClaim of an adopted session takes the SAME name, which is what
	// makes the lookup below possible at all.
	name := podspec.PodNameFor(req.Session)
	h := sandboxkinds.Handle{Kind: KindName, Ref: sandboxkinds.NamespacedRef(ns, name)}

	// An adopted session's Sandbox carries a POOL-GENERATED name this backend
	// cannot predict, so only the claim — at a name this backend chose — can
	// answer for one, and it MUST be consulted before eligibility is re-decided
	// below. A session's rendered PodSpec can change after the first Ensure (a
	// shared workspace stamped on later, toolchains frozen onto status), and a
	// re-entry that re-decided eligibility would find it ineligible and create
	// a second, cold sandbox alongside the one it already holds.
	//
	// A REFUSED claim is the one case that does not return here: pre-warming
	// must degrade, never break, so discardRefusedClaim deletes it and this
	// pass falls through to the cold path — which is also why adoption is
	// suppressed below rather than re-creating the claim just discarded.
	adoptionRefused := false
	if r.prewarmAvailable {
		b, berr := r.getClaimBinding(ctx, ns, name)
		if berr != nil {
			return sandboxkinds.Handle{}, berr
		}
		if b.found {
			if !claimRefused(b) {
				h.Prewarmed = true
				return h, nil
			}
			if err := r.discardRefusedClaim(ctx, req.Session, ns, name, b); err != nil {
				return sandboxkinds.Handle{}, err
			}
			adoptionRefused = true
		}
	}

	var existing sandboxv1beta1.Sandbox
	err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &existing)
	switch {
	case err == nil:
		return h, nil
	case !apierrors.IsNotFound(err):
		return sandboxkinds.Handle{}, fmt.Errorf("get sandbox %s/%s: %w", ns, name, err)
	}

	// Shared with the pod kind: waits for the claim to EXIST, never for it to
	// be Bound. Returns a wrapped ErrPreconditionPending when absent,
	// which the session controller reports as Progressing rather than a failure.
	if err := sandboxkinds.RequireWorkspaceClaim(ctx, r.deps.Client, req.Session); err != nil {
		return sandboxkinds.Handle{}, err
	}

	// The same builder the pod kind uses: this backend differs in the object it
	// wraps the PodSpec in, not in how the PodSpec is rendered.
	p, err := podspec.Build(req.Session, req.Class)
	if err != nil {
		return sandboxkinds.Handle{}, fmt.Errorf("build sandbox pod spec %s/%s: %w", ns, name, err)
	}

	// Take a ready sandbox out of the class's warm pool when this session may
	// safely have one. A false return is an ordinary cold start, not a failure.
	//
	// Skipped when this pass just discarded a refused claim: the session is
	// still eligible and the pool still exists, so re-entering tryAdopt would
	// re-create the rejected claim and spin create/refuse/delete forever. The
	// direct Sandbox below makes the decision stick — a later Ensure still runs
	// the claim lookup first, finds nothing, and returns on that Sandbox.
	//
	// THE LOOKUP ORDER IS LOAD-BEARING; do not "optimize" it to check the
	// Sandbox first. Upstream names a claim-created Sandbox after the claim,
	// which is the IDENTICAL name AP gives its direct Sandbox
	// (podspec.PodNameFor), so checking the Sandbox first would find an ADOPTED
	// session's sandbox and report it cold — and Teardown would then delete
	// that Sandbox while leaving the claim to provision a replacement.
	if !adoptionRefused {
		adopted, aerr := r.tryAdopt(ctx, req, p, name)
		if aerr != nil {
			return sandboxkinds.Handle{}, aerr
		}
		if adopted {
			h.Prewarmed = true
			return h, nil
		}
	}

	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			Labels:          p.Labels,
			OwnerReferences: p.OwnerReferences,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: p.Spec,
					ObjectMeta: sandboxv1beta1.PodMetadata{
						Labels:      p.Labels,
						Annotations: p.Annotations,
					},
				},
				// VolumeClaimTemplates is deliberately unset. It is
				// StatefulSet-style PER-SANDBOX storage; the shared workspace
				// claim is already a volume on the pod template above, where
				// every bundle of a session resolves it to the SAME PVC. The
				// CRD marks this field immutable, so a wrong value here could
				// not be corrected in place.
			},
			// Lifecycle and OperatingMode are deliberately unset: AP's TTL
			// controller owns expiry and its idle-sleep/reap controller owns
			// suspension. Two controllers racing to delete one sandbox is a bug.
		},
	}
	if err := r.deps.Client.Create(ctx, sb); err != nil && !apierrors.IsAlreadyExists(err) {
		return sandboxkinds.Handle{}, fmt.Errorf("create sandbox %s/%s: %w", ns, name, err)
	}
	return h, nil
}

// Status maps agent-sandbox conditions onto AP's vocabulary. Phase is the
// contract consumers switch on; Reason is surfaced but never branched on.
func (r *Runtime) Status(ctx context.Context, h sandboxkinds.Handle) (sandboxkinds.Status, error) {
	ns, name, err := sandboxkinds.ResolveHandle(h, KindName)
	if err != nil {
		return sandboxkinds.Status{}, err
	}

	// A pre-warmed handle names the CLAIM; only the claim's status knows the
	// pool-minted Sandbox behind it. Once resolved, the mapping below is
	// EXACTLY the cold path's — an adopted sandbox is an ordinary Sandbox, and
	// two condition mappings would be two sources of truth for one fact.
	var wantPodLabels map[string]string
	if h.Prewarmed {
		b, berr := r.getClaimBinding(ctx, ns, name)
		if berr != nil {
			return sandboxkinds.Status{}, berr
		}
		if !b.found {
			return sandboxkinds.Status{
				Phase:   sandboxkinds.PhaseGone,
				Reason:  sandboxkinds.ReasonGone,
				Message: fmt.Sprintf("sandbox claim %s/%s no longer exists", ns, name),
			}, nil
		}
		if b.sandboxName == "" {
			return unboundClaimStatus(ns, name, b), nil
		}
		name = b.sandboxName
		wantPodLabels = b.wantPodLabels
	}

	var sb sandboxv1beta1.Sandbox
	if err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sb); err != nil {
		if apierrors.IsNotFound(err) {
			return sandboxkinds.Status{
				Phase:   sandboxkinds.PhaseGone,
				Reason:  sandboxkinds.ReasonGone,
				Message: fmt.Sprintf("sandbox %s/%s no longer exists", ns, name),
			}, nil
		}
		return sandboxkinds.Status{}, fmt.Errorf("get sandbox %s/%s: %w", ns, name, err)
	}

	// Checked before any condition: a terminating sandbox will never accept
	// another exec, and reporting its last live phase would let a caller
	// dispatch a tool call into something shutting down.
	if sb.DeletionTimestamp != nil {
		return sandboxkinds.Status{
			Phase:   sandboxkinds.PhaseGone,
			Reason:  sandboxkinds.ReasonTerminating,
			Message: fmt.Sprintf("sandbox %s/%s is terminating", ns, name),
		}, nil
	}

	// Finished outranks Ready: a sandbox can carry a stale Ready=True alongside
	// a terminal Finished condition, and reading Ready first would report a
	// dead sandbox as usable.
	if f := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionFinished)); f != nil &&
		f.Status == metav1.ConditionTrue {
		switch f.Reason {
		case sandboxv1beta1.SandboxReasonPodFailed:
			return sandboxkinds.Status{
				Phase: sandboxkinds.PhaseFailed, Reason: sandboxkinds.ReasonCrashed,
				Message: conditionMessage(f, "sandbox pod failed"),
			}, nil
		default:
			// SandboxExpired and PodSucceeded both mean the sandbox reached its
			// end rather than failed at its job, and both stop serving exec.
			return sandboxkinds.Status{
				Phase: sandboxkinds.PhaseGone, Reason: sandboxkinds.ReasonGone,
				Message: conditionMessage(f, "sandbox finished"),
			}, nil
		}
	}

	if ready := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady)); ready != nil {
		if ready.Status == metav1.ConditionTrue {
			if h.Prewarmed {
				// A pooled Sandbox carries Ready=True from BEFORE it was adopted:
				// it was already running when the claim arrived. So Ready alone
				// does not mean this SESSION's sandbox is usable yet — see
				// adoptedPodLabelsLanded.
				st, landed, lerr := r.adoptedPodLabelsLanded(ctx, ns, &sb, wantPodLabels)
				if lerr != nil {
					return sandboxkinds.Status{}, lerr
				}
				if !landed {
					return st, nil
				}
			}
			return sandboxkinds.Status{Phase: sandboxkinds.PhaseReady, Reason: sandboxkinds.ReasonReady}, nil
		}
		if ready.Reason == sandboxv1beta1.SandboxReasonDependenciesNotReady {
			return sandboxkinds.Status{
				Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonWaitingForPrereq,
				Message: ready.Message,
			}, nil
		}
		return sandboxkinds.Status{
			Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonNotReady,
			Message: ready.Message,
		}, nil
	}

	return sandboxkinds.Status{
		Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonCreating,
		Message: "sandbox created; waiting for the agent-sandbox controller",
	}, nil
}

// ReasonAwaitingSessionLabels is Pending — an adopted sandbox whose pod has not
// yet received the session metadata AP's per-session NetworkPolicy selects on.
// A backend-specific reason rather than one of the shared vocabulary in
// pkg/tools/sandboxkinds: none of those names this fact, and the seam explicitly
// allows a backend its own string where none fits.
const ReasonAwaitingSessionLabels = "AwaitingSessionLabels"

// adoptedPodLabelsLanded reports whether the pod behind an ADOPTED sandbox has
// already received the metadata the claim asked the agent-sandbox controller to
// inject (spec.additionalPodMetadata), returning the Pending status to report
// when it has not.
//
// SECURITY, specifically a TIMING one. Adoption patches the CLAIM's spec; the
// agent-sandbox controller patches the already-running pod later, on its own
// queue. The pooled Sandbox carries Ready=True from before it was adopted, so
// without this check AP would report PhaseReady — and dispatch an exec — in the
// window between the two. In that window AP's per-session NetworkPolicy (which
// selects on agentprimitives.authzed.com/session) matches nothing and the
// template's NetworkPolicyManagement: Unmanaged means upstream created nothing
// either, leaving the pod under NO NetworkPolicy at all, even for a session set
// to networkMode: none.
//
// Both reads are cache-backed, so this costs no API round trip on the hot path.
//
// FAIL-CLOSED on an EMPTY want: a claim with no additionalPodMetadata describes
// a pod AP's NetworkPolicy can never select, so Ready is never safe to report.
// tryAdopt always injects podspec.Build's labels, so this is unreachable today
// — which is the point: an edit that stops injecting them yields a legible
// Pending instead of a silently unpoliced sandbox.
func (r *Runtime) adoptedPodLabelsLanded(
	ctx context.Context, ns string, sb *sandboxv1beta1.Sandbox, want map[string]string,
) (sandboxkinds.Status, bool, error) {
	pending := func(msg string) sandboxkinds.Status {
		return sandboxkinds.Status{
			Phase:   sandboxkinds.PhasePending,
			Reason:  ReasonAwaitingSessionLabels,
			Message: msg,
			// The ONE state this backend reports that none of its Watches() can
			// end. Pod and adopted Sandbox are owned by the SandboxClaim, so
			// neither maps back to the SpiceboxSession; the session-owned claim
			// does, but it goes Ready in the same pass that merely REQUESTS the
			// pod change, then has nothing further to say. Deliberately NOT set
			// on any other return here — those are watch-covered, and polling
			// per-phase would re-reconcile every starting sandbox on a timer.
			RequiresPolling: true,
		}
	}

	if len(want) == 0 {
		return pending(fmt.Sprintf(
			"adopted sandbox %s/%s: its claim asked for no pod metadata, so AP's per-session "+
				"NetworkPolicy can never select the pod", ns, sb.Name)), false, nil
	}

	podName := sb.Annotations[podNameAnnotation]
	if podName == "" {
		return pending(fmt.Sprintf(
			"adopted sandbox %s/%s has not recorded its pod yet (annotation %s is unset)",
			ns, sb.Name, podNameAnnotation)), false, nil
	}

	var pod corev1.Pod
	if err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return pending(fmt.Sprintf(
				"adopted sandbox %s/%s names pod %s, which is not visible yet", ns, sb.Name, podName)), false, nil
		}
		return sandboxkinds.Status{}, false, fmt.Errorf(
			"get pod %s/%s for adopted sandbox %s: %w", ns, podName, sb.Name, err)
	}

	for k, v := range want {
		if pod.Labels[k] != v {
			return pending(fmt.Sprintf(
				"adopted sandbox %s/%s: pod %s has not received label %s yet, so AP's per-session "+
					"NetworkPolicy does not select it", ns, sb.Name, podName, k)), false, nil
		}
	}
	return sandboxkinds.Status{}, true, nil
}

// conditionMessage prefers the condition's own message, falling back to a
// generic description when the backend leaves it empty. Shared by every arm
// that relays a backend condition — the Sandbox's and the SandboxClaim's alike.
func conditionMessage(c *metav1.Condition, fallback string) string {
	if c.Message != "" {
		return c.Message
	}
	return fallback
}

// podNameAnnotation is where the agent-sandbox controller records the pod
// backing a sandbox, aliased to the upstream constant so the literal string
// has one source of truth.
const podNameAnnotation = sandboxv1beta1.SandboxPodNameAnnotation

// Teardown deletes the Sandbox — or, for an adopted session, the SandboxClaim
// that owns it. An already-absent object is success: this runs from a
// finalizer that re-enters until it succeeds, so treating absence as an error
// would wedge session deletion forever.
func (r *Runtime) Teardown(ctx context.Context, h sandboxkinds.Handle) error {
	ns, name, err := sandboxkinds.ResolveHandle(h, KindName)
	if err != nil {
		return err
	}

	// Deleting the CLAIM is the whole teardown for an adopted session: upstream
	// makes the SandboxClaim controller the CONTROLLER owner of the Sandbox on
	// both paths, so cascade GC reclaims the Sandbox and its pod along with the
	// claim. Deleting the Sandbox by name first would need a second Get to
	// learn a name GC already knows, and would leave a window in which the
	// claim outlives its sandbox and provisions a replacement.
	//
	// IsNoMatchError is tolerated alongside IsNotFound: uninstalling the
	// extensions CRDs while adopted sessions are live makes Delete answer
	// NoKindMatch, and returning that would fail this finalizer on every pass,
	// leaving those sessions permanently undeletable. The uninstall already
	// removed every claim, so absence by deletion and absence by uninstall are
	// the same outcome.
	if h.Prewarmed {
		claim := &sandboxextv1beta1.SandboxClaim{}
		claim.Namespace, claim.Name = ns, name
		if err := r.deps.Client.Delete(ctx, claim); err != nil &&
			!apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete sandbox claim %s/%s: %w", ns, name, err)
		}
		return nil
	}

	sb := &sandboxv1beta1.Sandbox{}
	sb.Namespace, sb.Name = ns, name
	if err := r.deps.Client.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete sandbox %s/%s: %w", ns, name, err)
	}
	return nil
}

// Executor returns the exec transport bound to this sandbox's pod.
//
// The pod is read from the annotation the agent-sandbox controller stamps,
// not from status.selector: one resolution path that can be absent beats two
// that can disagree about which pod backs a sandbox. The container name comes
// from podspec because podspec.Build rendered this pod template.
func (r *Runtime) Executor(h sandboxkinds.Handle) (exec.Executor, error) {
	ns, name, err := sandboxkinds.ResolveHandle(h, KindName)
	if err != nil {
		return nil, err
	}
	if r.deps.ExecFor == nil {
		return nil, fmt.Errorf("agent-sandbox runtime has no exec transport configured")
	}

	// Same claim indirection Status takes, but fail-closed rather than
	// Pending: a claim with no sandbox yet has no exec target at all, and
	// binding to an unresolved one would dispatch a tool call at nothing and
	// fail somewhere far from the real cause.
	if h.Prewarmed {
		b, err := r.getClaimBinding(context.Background(), ns, name)
		if err != nil {
			return nil, err
		}
		if !b.found {
			return nil, fmt.Errorf("sandbox claim %s/%s no longer exists", ns, name)
		}
		if b.sandboxName == "" {
			return nil, fmt.Errorf(
				"sandbox claim %s/%s has not been given a sandbox yet", ns, name)
		}
		name = b.sandboxName
	}

	var sb sandboxv1beta1.Sandbox
	if err := r.deps.Client.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, &sb); err != nil {
		return nil, fmt.Errorf("get sandbox %s/%s: %w", ns, name, err)
	}
	podName := sb.Annotations[podNameAnnotation]
	if podName == "" {
		return nil, fmt.Errorf(
			"sandbox %s/%s has no pod yet (annotation %s is unset)", ns, name, podNameAnnotation)
	}
	return r.deps.ExecFor(ns, podName, podspec.ContainerName), nil
}

// Watches contributes the owned-Sandbox watch. Only reached for a kind whose
// NewRuntime succeeded, which is what keeps this informer off clusters lacking
// the CRD -- registering one there wedges the manager at "failed to wait for
// caches to sync".
//
// The SandboxClaim watch is REQUIRED for an adopted session to converge and is
// not covered by the Sandbox watch: upstream makes the CLAIM the
// controller-owner of an adopted Sandbox, so Owns(&Sandbox{}) can never map it
// back to the SpiceboxSession, while the session-owned claim does. Without it
// an adopted session sits at "waiting for readiness" until the manager's
// 10-hour resync, since the reconcile returns no RequeueAfter.
//
// Gated on the same availability bit Ensure uses: the extensions CRDs ship
// separately, and an unconditional informer on an absent one crash-loops the
// operator and takes every other controller down with it.
func (r *Runtime) Watches() []sandboxkinds.Watch {
	w := []sandboxkinds.Watch{{Object: &sandboxv1beta1.Sandbox{}}}
	if r.prewarmAvailable {
		w = append(w, sandboxkinds.Watch{Object: &sandboxextv1beta1.SandboxClaim{}})
	}
	return w
}

var _ sandboxkinds.Runtime = (*Runtime)(nil)
