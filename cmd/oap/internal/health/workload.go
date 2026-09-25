package health

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

type workloadKind int

const (
	kindDeployment workloadKind = iota
	kindStatefulSet
)

// workload is the common-case component: a Deployment or StatefulSet that is
// healthy only when every desired replica is updated AND ready with none
// unavailable. Checking updated+unavailable (not just ReadyReplicas) is what
// catches a stuck rollout — a new pod crash-looping while the old replica stays
// Ready — which a bare ReadyReplicas>=1 check masks (exactly how `oap check`
// passed while the operator's new pod was CrashLoopBackOff).
type workload struct {
	name     string
	kind     workloadKind
	optional bool
}

// Deployment registers a Deployment component. optional=true means a not-ready
// state is reported as Optional (warn), not Failed.
func Deployment(name string, optional bool) Component {
	return workload{name: name, kind: kindDeployment, optional: optional}
}

// StatefulSet registers a StatefulSet component.
func StatefulSet(name string, optional bool) Component {
	return workload{name: name, kind: kindStatefulSet, optional: optional}
}

func (w workload) Name() string { return w.name }

func (w workload) notReady() Status {
	if w.optional {
		return Optional
	}
	return Failed
}

func (w workload) Check(ctx context.Context, b *kube.Bundle) Result {
	var want, ready, updated, unavailable int32
	var selector *metav1.LabelSelector

	switch w.kind {
	case kindDeployment:
		d, err := b.Typed.AppsV1().Deployments(namespace).Get(ctx, w.name, metav1.GetOptions{})
		if err != nil {
			return Result{Status: w.notReady(), Detail: fmt.Sprintf("deployment not found: %v", err), NotFound: apierrors.IsNotFound(err)}
		}
		want = desired(d.Spec.Replicas)
		ready, updated, unavailable = d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.UnavailableReplicas
		selector = d.Spec.Selector
	case kindStatefulSet:
		s, err := b.Typed.AppsV1().StatefulSets(namespace).Get(ctx, w.name, metav1.GetOptions{})
		if err != nil {
			return Result{Status: w.notReady(), Detail: fmt.Sprintf("statefulset not found: %v", err), NotFound: apierrors.IsNotFound(err)}
		}
		want = desired(s.Spec.Replicas)
		ready, updated = s.Status.ReadyReplicas, s.Status.UpdatedReplicas
		selector = s.Spec.Selector
	}

	if ready >= want && updated >= want && unavailable == 0 {
		return Result{Status: OK, Detail: fmt.Sprintf("%d/%d ready", ready, want)}
	}
	detail := fmt.Sprintf("%d/%d ready", ready, want)
	if reason := w.podTrouble(ctx, b, selector); reason != "" {
		detail += " (" + reason + ")"
	}
	return Result{Status: w.notReady(), Detail: detail}
}

// podTrouble returns the first waiting-state reason among the workload's pods
// (e.g. "CrashLoopBackOff", "ImagePullBackOff", "CreateContainerConfigError")
// so the failure detail names WHY, not just the ready count. Best-effort: a
// list error or no waiting container yields "".
func (w workload) podTrouble(ctx context.Context, b *kube.Bundle, selector *metav1.LabelSelector) string {
	if selector == nil {
		return ""
	}
	pods, err := b.Typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(selector),
	})
	if err != nil {
		return ""
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
				return cs.State.Waiting.Reason
			}
		}
	}
	return ""
}

// Repair triggers a rollout restart by stamping the pod template with a fresh
// annotation (the same mechanism as `kubectl rollout restart`). It clears a
// transient crash/race; it cannot fix a genuine config/code fault (the new pods
// will crash again), so `oap check --repair` re-checks afterward rather than
// assuming success. Only invoked on a Failed component.
func (w workload) Repair(ctx context.Context, b *kube.Bundle) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"ap.authzed.com/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339Nano)))
	var err error
	switch w.kind {
	case kindDeployment:
		_, err = b.Typed.AppsV1().Deployments(namespace).Patch(ctx, w.name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	case kindStatefulSet:
		_, err = b.Typed.AppsV1().StatefulSets(namespace).Patch(ctx, w.name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		return fmt.Errorf("rollout restart %s: %w", w.name, err)
	}
	return nil
}

func desired(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}
