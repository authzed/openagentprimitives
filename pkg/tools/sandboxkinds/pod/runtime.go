package pod

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/platform/podstatus"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// Runtime is the pod backend's lifecycle implementation. It owns no state of
// its own beyond its injected dependencies; the sandbox's identity travels in
// the Handle.
type Runtime struct {
	deps sandboxkinds.Deps
}

// Ensure creates the sandbox pod if absent and returns its handle. Idempotent:
// an existing pod (or a concurrent creator winning the race) yields the same
// handle rather than a second sandbox.
func (r *Runtime) Ensure(ctx context.Context, req sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	if req.Session == nil {
		return sandboxkinds.Handle{}, fmt.Errorf("EnsureRequest.Session is required")
	}
	ns := req.Session.Namespace
	name := podspec.PodNameFor(req.Session)
	h := sandboxkinds.Handle{Kind: KindName, Ref: sandboxkinds.NamespacedRef(ns, name)}

	var existing corev1.Pod
	err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &existing)
	switch {
	case err == nil:
		return h, nil
	case !apierrors.IsNotFound(err):
		return sandboxkinds.Handle{}, fmt.Errorf("get sandbox pod %s/%s: %w", ns, name, err)
	}

	if err := sandboxkinds.RequireWorkspaceClaim(ctx, r.deps.Client, req.Session); err != nil {
		return sandboxkinds.Handle{}, err
	}

	p, err := podspec.Build(req.Session, req.Class)
	if err != nil {
		return sandboxkinds.Handle{}, fmt.Errorf("build sandbox pod %s/%s: %w", ns, name, err)
	}
	if err := r.deps.Client.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
		return sandboxkinds.Handle{}, fmt.Errorf("create sandbox pod %s/%s: %w", ns, name, err)
	}
	return h, nil
}

// Status maps pod phase onto AP's vocabulary. A pod that no longer exists is
// PhaseGone rather than an error: a completed or reaped session re-enters here
// on every reconcile and must not produce an error each time.
func (r *Runtime) Status(ctx context.Context, h sandboxkinds.Handle) (sandboxkinds.Status, error) {
	ns, name, err := sandboxkinds.ResolveHandle(h, KindName)
	if err != nil {
		return sandboxkinds.Status{}, err
	}

	var p corev1.Pod
	if err := r.deps.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return sandboxkinds.Status{
				Phase:   sandboxkinds.PhaseGone,
				Reason:  sandboxkinds.ReasonGone,
				Message: fmt.Sprintf("sandbox pod %s/%s no longer exists", ns, name),
			}, nil
		}
		return sandboxkinds.Status{}, fmt.Errorf("get sandbox pod %s/%s: %w", ns, name, err)
	}

	// A terminating pod is Gone for every purpose this status serves: it will
	// never accept another exec, and reporting its last phase would let a
	// caller dispatch a tool call into a sandbox that is shutting down. The pod
	// object outlives the decision — it lingers until the kubelet confirms, and
	// under envtest (no kubelet) it lingers indefinitely.
	if p.DeletionTimestamp != nil {
		return sandboxkinds.Status{
			Phase:   sandboxkinds.PhaseGone,
			Reason:  sandboxkinds.ReasonTerminating,
			Message: fmt.Sprintf("sandbox pod %s/%s is terminating", ns, name),
		}, nil
	}

	// A pod wedged in a terminal container-waiting state (ImagePullBackOff,
	// CreateContainerConfigError, CrashLoopBackOff) never reaches the Failed
	// PHASE — the kubelet retries forever — so the phase switch below misses it.
	// Without this the session sits Pending indefinitely and whatever waits on it
	// hangs with no signal.
	if reason, msg, ok := podstatus.TerminalContainerWaitReason(&p); ok {
		detail := reason
		if msg != "" {
			detail = reason + " — " + msg
		}
		return sandboxkinds.Status{
			Phase: sandboxkinds.PhaseFailed, Reason: sandboxkinds.ReasonStartFailed,
			Message: detail,
		}, nil
	}

	switch p.Status.Phase {
	case corev1.PodRunning:
		if podReady(&p) {
			return sandboxkinds.Status{Phase: sandboxkinds.PhaseReady, Reason: sandboxkinds.ReasonReady}, nil
		}
		return sandboxkinds.Status{
			Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonNotReady,
			Message: "sandbox is running but not yet ready",
		}, nil
	case corev1.PodFailed:
		reason := sandboxkinds.ReasonCrashed
		if oomKilled(&p) {
			reason = sandboxkinds.ReasonOOMKilled
		}
		return sandboxkinds.Status{Phase: sandboxkinds.PhaseFailed, Reason: reason, Message: podFailureMessage(&p)}, nil
	case corev1.PodSucceeded:
		// restartPolicy:Never means a completed sandbox never serves exec again.
		return sandboxkinds.Status{
			Phase: sandboxkinds.PhaseGone, Reason: sandboxkinds.ReasonGone,
			Message: "sandbox completed",
		}, nil
	default:
		return sandboxkinds.Status{
			Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonCreating,
			Message: string(p.Status.Phase),
		}, nil
	}
}

// Teardown deletes the sandbox pod. An already-absent pod is success.
func (r *Runtime) Teardown(ctx context.Context, h sandboxkinds.Handle) error {
	ns, name, err := sandboxkinds.ResolveHandle(h, KindName)
	if err != nil {
		return err
	}
	p := &corev1.Pod{}
	p.Namespace, p.Name = ns, name
	if err := r.deps.Client.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete sandbox pod %s/%s: %w", ns, name, err)
	}
	return nil
}

// Executor returns the exec transport bound to this sandbox's container.
func (r *Runtime) Executor(h sandboxkinds.Handle) (exec.Executor, error) {
	ns, name, err := sandboxkinds.ResolveHandle(h, KindName)
	if err != nil {
		return nil, err
	}
	if r.deps.ExecFor == nil {
		return nil, fmt.Errorf("pod sandbox runtime has no exec transport configured")
	}
	return r.deps.ExecFor(ns, name, podspec.ContainerName), nil
}

// Watches contributes the owned-pod watch a controller needs to observe
// sandbox state changes.
func (r *Runtime) Watches() []sandboxkinds.Watch {
	return []sandboxkinds.Watch{{Object: &corev1.Pod{}}}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podFailureMessage prefers the terminated container's message, which
// terminationMessagePolicy=FallbackToLogsOnError populates with the tail of the
// container's logs, over the pod-level reason.
func podFailureMessage(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.Message != "" {
			return t.Message
		}
	}
	if p.Status.Message != "" {
		return p.Status.Message
	}
	return "sandbox pod failed"
}

var _ sandboxkinds.Runtime = (*Runtime)(nil)
