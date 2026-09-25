package cloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// WorkspaceVerify controls how the cmd/oap orchestration handles the detected
// storage class before trusting it for shared-workspace PVCs.
type WorkspaceVerify int

const (
	WorkspaceProbeBeforeUse       WorkspaceVerify = iota // cheap class: provisioning-probe; use iff it binds
	WorkspaceCostConfirmBeforeUse                        // billable managed RWX (Filestore): warn + confirm, then trust
	WorkspaceUseDirectly                                 // explicit override or already-verified marker
)

// Decision is the per-cloud workspace-storage outcome. The side-effecting probe
// (ProbeBeforeUse) and interactive confirm (CostConfirmBeforeUse) are performed
// by the cmd/oap orchestration driven by Verify — Resolve itself is pure detection.
type Decision struct {
	ClassName    string // candidate RWX class; "" ⇒ isolated
	NeedsBundled bool   // caller applies the bundled local-path provisioner before probing
	Verify       WorkspaceVerify
	CostWarning  string // CostConfirmBeforeUse: the $/latency warning to show
	Degraded     bool   // no usable RWX on this cluster
	Message      string // summary / degrade guidance (loud)
}

// WorkspaceParams carries the inputs WorkspaceStorage.Resolve needs.
type WorkspaceParams struct {
	Clients  Clients
	Reporter Reporter
}

// WorkspaceStorage decides the RWX storage class for AgentSession
// shared-workspace PVCs on a given cloud.
type WorkspaceStorage interface {
	// Resolve decides the RWX class for AgentSession shared-workspace PVCs on this
	// cloud from existing cluster state. Pure detection — no side effects.
	Resolve(ctx context.Context, p WorkspaceParams) (Decision, error)
}

// WorkspaceEnableParams carries what an enable step needs. Mirrors the
// GatewayControllerParams shape so cmd/oap wires it the same way.
type WorkspaceEnableParams struct {
	Clients   Clients
	Reporter  Reporter
	In        io.Reader // stdin for the consent prompt
	AssumeYes bool
}

// WorkspaceStorageEnabler is an OPTIONAL capability a WorkspaceStorage may
// implement: a side-effecting, consent-gated step run BEFORE Resolve that makes
// a durable RWX class available (GKE offers to enable the Filestore CSI addon).
// It is NOT on the Strategy interface because only GKE has such a step — the
// other clouds have no addon to enable — so cmd/oap type-asserts for it and
// skips when absent, rather than forcing four no-op stubs. Idempotent (a class
// already present is a no-op); a decline / non-interactive run / underivable
// cluster identity returns nil and leaves Resolve to fall back.
type WorkspaceStorageEnabler interface {
	EnsureWorkspaceStorage(ctx context.Context, p WorkspaceEnableParams) error
}

// BundledWorkspaceStorageClass is the StorageClass name of the bundled
// local-path RWX provisioner that oap install applies on clusters that
// support hostPath volumes (GKE Standard, kind, bare-metal). GKE Autopilot
// rejects hostPath, so the GKE workspace strategy falls through to Filestore
// or degrades to isolated rather than using this class.
const BundledWorkspaceStorageClass = "ap-workspace-rwx"

// workspaceProbeNamespace is the namespace used for provisioning-probe resources.
// Mirrors the agentprimitives-system namespace where workspace config lives.
const workspaceProbeNamespace = "agentprimitives-system"

// knownRWXProvisioners is the allow-list of provisioner names that reliably
// support ReadWriteMany. Sources: CSI drivers in widespread production use.
// Cluster admins with custom RWX provisioners can bypass detection with an
// explicit StorageClass flag.
var knownRWXProvisioners = map[string]bool{
	"nfs.csi.k8s.io":               true,
	"efs.csi.aws.com":              true,
	"cephfs.csi.ceph.com":          true,
	"driver.longhorn.io":           true,
	"file.csi.azure.com":           true,
	"filestore.csi.storage.gke.io": true,
}

// IsGKEAutopilot reports whether the target cluster is GKE Autopilot, detected
// kube-only via the presence of the Autopilot "Warden" validating admission
// webhook — the same webhook that rejects hostPath/privileged workloads
// (autogke-no-write-mode-hostpath). On Autopilot the bundled local-path
// workspace provisioner cannot run, so callers must use a managed RWX class
// (Filestore) or degrade to isolated.
func IsGKEAutopilot(ctx context.Context, kc kubernetes.Interface) (bool, error) {
	const wardenWebhook = "warden-validating.common-webhooks.networking.gke.io"
	_, err := kc.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, wardenWebhook, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("detect GKE Autopilot (get warden webhook): %w", err)
	}
}

// latestProvisioningFailure returns the message from the most recent Warning
// event for the given object name, or "" if no such event exists. Both probe
// steps read it: for the PVC (ProvisioningFailed) and for the consumer pod
// (FailedAttachVolume).
func latestProvisioningFailure(ctx context.Context, kc kubernetes.Interface, ns, pvcName string) (string, error) {
	events, err := kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + pvcName,
	})
	if err != nil {
		return "", fmt.Errorf("list events for PVC %s/%s: %w", ns, pvcName, err)
	}
	var latest *corev1.Event
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.Type != corev1.EventTypeWarning {
			continue
		}
		if latest == nil || ev.LastTimestamp.After(latest.LastTimestamp.Time) {
			latest = ev
		}
	}
	if latest == nil {
		return "", nil
	}
	return latest.Message, nil
}

// ProbeStep polls whether a provisioning test has reached its ready condition:
// the PVC bound (the default), or — when NewProvisioningProbe was given
// WaitForPodRunning — the consumer pod actually Running (volume attached +
// mounted). ready=true means that condition is met. ready=false with a
// non-empty transientReason means provisioning is still in progress (e.g. a
// ProvisioningFailed or FailedAttachVolume event the controller may retry).
// err is reserved for hard failures (API unreachable, context cancellation).
type ProbeStep func(ctx context.Context) (ready bool, transientReason string, err error)

// ProbeOptions tunes NewProvisioningProbe. The zero value reproduces the
// original workspace behavior: an RWX PVC whose readiness is "Bound".
type ProbeOptions struct {
	// AccessMode is the probe PVC's access mode. "" defaults to ReadWriteMany
	// (workspace RWX). Stateful callers pass ReadWriteOnce.
	AccessMode corev1.PersistentVolumeAccessMode
	// WaitForPodRunning makes the probe report ready only when the consumer pod
	// reaches Running — i.e. the volume actually attached + mounted. RWO block
	// storage can BIND on an incompatible node yet fail to ATTACH
	// (FailedAttachVolume); the Bound-only check would falsely pass, so stateful
	// callers set this true.
	WaitForPodRunning bool
}

func (o ProbeOptions) accessMode() corev1.PersistentVolumeAccessMode {
	if o.AccessMode == "" {
		return corev1.ReadWriteMany
	}
	return o.AccessMode
}

// NewProvisioningProbe creates a test PVC and consumer Pod for StorageClass
// class once and returns a ProbeStep that checks the PVC's phase on each
// call, plus a cleanup func that deletes both resources. The cleanup must be
// deferred by the caller regardless of outcome.
//
// The probe imposes no internal timeout — the caller's context and the
// surrounding progress.Phase.Await own the patience policy, so a user choosing
// to keep waiting is not cut off by a deadline baked in here. A transient
// ProvisioningFailed event surfaces as transientReason (the provisioner may
// retry and the PVC may still bind), NOT as a terminal failure.
//
// opts.WaitForPodRunning makes the step report ready only once the consumer pod
// reaches Running, catching RWO FailedAttachVolume that a Bound-only check
// misses.
func NewProvisioningProbe(ctx context.Context, kc kubernetes.Interface, class string, opts ProbeOptions) (ProbeStep, func(), error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, nil, fmt.Errorf("generate probe names: %w", err)
	}
	suffix := hex.EncodeToString(b)
	pvcName := "ap-rwx-probe-" + suffix
	podName := "ap-rwx-probe-" + suffix
	ns := workspaceProbeNamespace

	storage := resource.MustParse("10Mi")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
			Labels: map[string]string{
				InstallTierLabelKey: InstallTierWorkspaceProbe,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{opts.accessMode()},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
			},
		},
	}
	if _, cerr := kc.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{}); cerr != nil {
		return nil, nil, fmt.Errorf("create probe PVC: %w", cerr)
	}

	cpuReq := resource.MustParse("10m")
	memReq := resource.MustParse("16Mi")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				InstallTierLabelKey: InstallTierWorkspaceProbe,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{
				{
					Name:    "probe",
					Image:   "busybox:1.36",
					Command: []string{"sh", "-c", "sleep 600"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    cpuReq,
							corev1.ResourceMemory: memReq,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    cpuReq,
							corev1.ResourceMemory: memReq,
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/mnt"},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
						RunAsNonRoot:           ptr.To(true),
						RunAsUser:              ptr.To(int64(65534)),
						ReadOnlyRootFilesystem: ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
	if _, cerr := kc.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); cerr != nil {
		// Best-effort PVC cleanup before returning the error. Surface a cleanup
		// failure alongside the create error so neither is silently dropped.
		if derr := kc.CoreV1().PersistentVolumeClaims(ns).Delete(context.Background(), pvcName, metav1.DeleteOptions{}); derr != nil {
			return nil, nil, fmt.Errorf("create probe Pod: %w (probe PVC %s cleanup also failed: %v)", cerr, pvcName, derr)
		}
		return nil, nil, fmt.Errorf("create probe Pod: %w", cerr)
	}

	cleanup := func() {
		_ = kc.CoreV1().Pods(ns).Delete(context.Background(), podName, metav1.DeleteOptions{})
		_ = kc.CoreV1().PersistentVolumeClaims(ns).Delete(context.Background(), pvcName, metav1.DeleteOptions{})
	}

	step := ProbeStep(func(pctx context.Context) (bool, string, error) {
		if opts.WaitForPodRunning {
			return probePodRunning(pctx, kc, ns, podName)
		}
		return probePVCBound(pctx, kc, ns, pvcName, podName)
	})

	return step, cleanup, nil
}

// probePVCBound reports the "ready when the PVC is Bound" semantics, surfacing
// the most actionable transient reason: the PVC's latest ProvisioningFailed
// Warning, or — when the provisioner has recorded none yet — a synthesized
// explanation naming the PVC's phase and the consumer pod's not-ready detail.
//
// The fallback is load-bearing. awaitProvisioningProbe turns a non-empty reason
// into the ONLY diagnosis an install prints for this phase and drops an empty
// one on the floor, so returning "" leaves the user watching "workspace storage
// <class>" hang with nothing said about why. WaitForFirstConsumer binding
// produces exactly that shape when the consumer pod is unschedulable: the PVC
// sits Pending and no provisioning event is ever recorded.
func probePVCBound(ctx context.Context, kc kubernetes.Interface, ns, pvcName, podName string) (bool, string, error) {
	cur, gerr := kc.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if gerr != nil {
		return false, "", fmt.Errorf("poll probe PVC %s: %w", pvcName, gerr)
	}
	if cur.Status.Phase == corev1.ClaimBound {
		return true, "", nil
	}
	reason, rerr := latestProvisioningFailure(ctx, kc, ns, pvcName)
	if rerr != nil {
		return false, "", rerr
	}
	if reason != "" {
		return false, reason, nil
	}
	return false, unboundPVCDetail(ctx, kc, ns, podName, cur.Status.Phase), nil
}

// unboundPVCDetail synthesizes the never-empty explanation probePVCBound falls
// back to. The consumer pod is where the real cause usually shows up (an
// unschedulable "Insufficient cpu", a stuck ContainerCreating); a Get failure
// on it only costs that clause, since the PVC phase alone still says more than
// silence.
func unboundPVCDetail(ctx context.Context, kc kubernetes.Interface, ns, podName string, phase corev1.PersistentVolumeClaimPhase) string {
	if phase == "" {
		phase = corev1.ClaimPending
	}
	detail := fmt.Sprintf("PVC still %s", phase)
	if pod, gerr := kc.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{}); gerr == nil {
		if d := podNotReadyDetail(pod); d != "" {
			detail += "; consumer pod " + d
		}
	}
	return detail + " (no provisioning-failure event recorded — the provisioner may still be working; check its logs / helper pods)"
}

// probePodRunning reports ready only when the consumer pod is Running (the
// volume attached + mounted). When it is not, it surfaces the most actionable
// transient reason: the pod's latest Warning event (e.g. FailedAttachVolume) or
// its not-ready detail. This is what catches the RWO attach failure that the
// Bound-only check misses.
func probePodRunning(ctx context.Context, kc kubernetes.Interface, ns, podName string) (bool, string, error) {
	pod, gerr := kc.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if gerr != nil {
		return false, "", fmt.Errorf("poll probe pod %s: %w", podName, gerr)
	}
	if pod.Status.Phase == corev1.PodRunning {
		return true, "", nil
	}
	reason, rerr := latestProvisioningFailure(ctx, kc, ns, podName)
	if rerr != nil {
		return false, "", rerr
	}
	if reason == "" {
		reason = podNotReadyDetail(pod)
	}
	return false, reason, nil
}

// podNotReadyDetail returns the most actionable reason a probe consumer Pod is not
// running: an unschedulable condition (e.g. Insufficient cpu) first, then a
// container Waiting reason (e.g. ContainerCreating / ImagePullBackOff), else the
// raw phase. "" only for a pod with no usable status yet.
func podNotReadyDetail(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
			return "unschedulable: " + c.Message
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			if w.Message != "" {
				return w.Reason + ": " + w.Message
			}
			return w.Reason
		}
	}
	if pod.Status.Phase != "" {
		return "phase " + string(pod.Status.Phase)
	}
	return ""
}

// ResolveRWXOrBundled is the workspace-storage decision every cloud EXCEPT GKE
// makes: prefer a class backed by this cloud's own RWX driver, and otherwise
// fall back to the bundled local-path provisioner. Either way the caller
// probes before trusting the class, because a class existing is not evidence
// that it can provision here.
//
// preferred names the cloud's native RWX provisioners in priority order; pass
// nil on a cloud that has none (local, unmanaged), which leaves FindRWXClass's
// generic known-provisioner scan.
//
// GKE does not use this: Autopilot cannot run the bundled hostPath
// provisioner, so it needs Filestore's cost confirmation and a Degraded arm
// that has no equivalent here.
func ResolveRWXOrBundled(ctx context.Context, kc kubernetes.Interface, preferred []string) (Decision, error) {
	cls, err := FindRWXClass(ctx, kc, preferred)
	if err != nil {
		return Decision{}, err
	}
	if cls != "" {
		return Decision{ClassName: cls, Verify: WorkspaceProbeBeforeUse}, nil
	}
	return Decision{ClassName: BundledWorkspaceStorageClass, NeedsBundled: true, Verify: WorkspaceProbeBeforeUse}, nil
}

// FindRWXClass scans the cluster's StorageClasses and returns the first one
// whose provisioner appears in the preferred list; if none match, it falls
// back to the first class whose provisioner is in knownRWXProvisioners; if
// still none, it returns "". The preferred list lets the caller express
// cloud-native ordering without hard-coding class names.
func FindRWXClass(ctx context.Context, kc kubernetes.Interface, preferred []string) (string, error) {
	scs, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list storage classes: %w", err)
	}
	prefSet := make(map[string]bool, len(preferred))
	for _, p := range preferred {
		prefSet[p] = true
	}
	var prefMatch, knownMatch string
	for i := range scs.Items {
		sc := &scs.Items[i]
		if prefSet[sc.Provisioner] && prefMatch == "" {
			prefMatch = sc.Name
		}
		if knownRWXProvisioners[sc.Provisioner] && knownMatch == "" {
			knownMatch = sc.Name
		}
	}
	if prefMatch != "" {
		return prefMatch, nil
	}
	return knownMatch, nil
}

// FindPreferredRWXClass lists StorageClasses for the given provisioner and
// splits them into the first whose parameters[paramKey]==paramVal (preferred)
// and the first that lacks it (fallback). Either may be "".
//
// GKE uses this to prefer a Filestore MULTISHARE class (parameters.multishare=
// "true": one Filestore instance shared across many PVCs) over a per-PVC class,
// which provisions a dedicated ≥1 TiB instance for EACH claim — ruinous with
// many small session workspaces. FindRWXClass returns the first class by name
// order, which cannot tell the two apart, so a per-PVC class sorting earlier
// would silently be chosen; this reads the discriminating parameter instead.
func FindPreferredRWXClass(ctx context.Context, kc kubernetes.Interface, provisioner, paramKey, paramVal string) (preferred, fallback string, err error) {
	scs, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("list storage classes: %w", err)
	}
	for i := range scs.Items {
		sc := &scs.Items[i]
		if sc.Provisioner != provisioner {
			continue
		}
		if sc.Parameters[paramKey] == paramVal {
			if preferred == "" {
				preferred = sc.Name
			}
		} else if fallback == "" {
			fallback = sc.Name
		}
	}
	return preferred, fallback, nil
}
