// Package sidecartoolbox — probe.go: ephemeral probe-Pod helpers.
package sidecartoolbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sidecartoolboxsynth "github.com/authzed/openagentprimitives/pkg/agent/tool/sidecartoolbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/netpolrule"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

const (
	probePort          = 8080
	probeContainerName = "probe"

	// defaultProbeTimeoutSeconds applies when the CR leaves
	// healthcheck.timeoutSeconds at zero. The CRD defaults the field to 30, so
	// this only covers structs built in-process.
	defaultProbeTimeoutSeconds = 60
)

// probeTimeoutSeconds is the single source for how long probeOnce waits for the
// probe Pod AND how long that Pod is allowed to live. Both must derive from the
// same number: healthcheck.timeoutSeconds has no CRD maximum, so a Pod pinned to
// a smaller fixed deadline is killed by the kubelet while the loop is still
// polling — and, because the loop watches only PodReady and never Status.Phase,
// the CR is then told it "did not become Ready in time" for a Pod the operator
// itself terminated.
func probeTimeoutSeconds(cr *spiceboxv1alpha1.SidecarToolbox) int64 {
	if t := cr.Spec.Transport.Healthcheck.TimeoutSeconds; t > 0 {
		return int64(t)
	}
	return defaultProbeTimeoutSeconds
}

// ProbeResult bundles the outputs of a successful probe run.
type ProbeResult struct {
	Tools  []probe.Tool
	Digest string // kubelet-resolved image digest ("sha256:…"), empty if unavailable
}

// probeOnce creates a one-shot Pod, waits for ready, calls tools/list,
// and returns the live tool list and the kubelet-resolved image digest.
// Pod has restartPolicy=Never and is owned by the SidecarToolbox CR;
// it's torn down explicitly after use.
//
// The Pod runs the same image+command the runtime sidecar would, with
// MCP_PORT=8080. The probe reaches the Pod via PodIP:8080 (in-cluster
// pod network).
// probeReader returns the uncached APIReader when wired (production), falling
// back to the cached Client (unit tests). The probe Pod is freshly created and
// changes fast, so it must be read from the API server, not the lagging cache.
func (r *Reconciler) probeReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// dialAndList lists tools from a Ready probe Pod's MCP endpoint. The dialProbe
// seam lets tests simulate the fresh-pod window without a live pod; production
// builds a plain probe.Client (no safehttp SSRF guard — the target is a
// Kubernetes-assigned Pod IP, not an LLM/user-supplied URL).
func (r *Reconciler) dialAndList(ctx context.Context, url string) ([]probe.Tool, error) {
	if r.dialProbe != nil {
		return r.dialProbe(ctx, url)
	}
	pc := &probe.Client{HTTP: &http.Client{}, URL: url}
	return pc.ListTools(ctx, "", "")
}

func (r *Reconciler) probeOnce(ctx context.Context, cr *spiceboxv1alpha1.SidecarToolbox) (ProbeResult, error) {
	logger := log.FromContext(ctx)
	podName := "probe-" + cr.Name + "-" + time.Now().Format("20060102-150405")
	if r.ProbeNetpol.Enabled {
		// BEFORE the pod: the policy must already admit the operator by the
		// time the image starts answering, or the dial window shrinks by
		// however long the policy takes to land.
		if err := r.ensureProbeNetpol(ctx, cr); err != nil {
			logger.Info("sidecartoolbox: failed to converge the probe NetworkPolicy; probing anyway (the dial will fail closed if ingress stays denied)",
				"toolbox", cr.Name, "err", err.Error())
		}
	}
	pod := r.buildProbePod(cr, podName)
	if err := r.Client.Create(ctx, pod); err != nil {
		// The returned error becomes the Reachable=False condition MESSAGE, which
		// the controller persists. It MUST be a pure function of the failure class
		// — no timestamped pod name, no pod IP — or every reconcile mints a new
		// probe pod (new name/IP), writes a different message, and the controller's
		// own status watch re-fires immediately: a hot loop spawning a probe pod
		// every couple of seconds. Volatile detail goes to the log instead.
		logger.Info("sidecartoolbox: failed to create probe pod",
			"toolbox", cr.Name, "pod", podName, "err", err.Error())
		return ProbeResult{}, errors.New("failed to create reachability probe pod")
	}
	defer func() {
		if delErr := r.Client.Delete(context.Background(), pod); delErr != nil && !apierrors.IsNotFound(delErr) {
			logger.Info("sidecartoolbox: failed to delete probe pod",
				"pod", pod.Name, "namespace", pod.Namespace, "err", delErr.Error())
		}
	}()

	deadline := time.Now().Add(time.Duration(probeTimeoutSeconds(cr)) * time.Second)
	// sawPod distinguishes the two NotFound cases below: BEFORE the first
	// sighting, a NotFound is informer-cache lag (Create hit the API server, but
	// this cache-served Get has not caught up yet) and must be waited out; only
	// AFTER we have seen the pod does a NotFound mean it genuinely vanished.
	sawPod := false
	// lastDialErr holds the most recent MCP-dial failure. A probe pod can report
	// Ready (kubelet runs the readiness httpGet from the node's own netns) a beat
	// before its endpoint is reachable cross-pod from the operator pod (CNI/route
	// programming lag for a just-scheduled pod), surfacing as a transient
	// "connection refused". We retry the dial within the deadline rather than
	// failing the whole probe on the first refusal — a stable pod serves the same
	// endpoint fine. lastDialErr only steers the STABLE terminal message; the
	// volatile pod IP it carries is logged, never returned.
	var lastDialErr error
	for time.Now().Before(deadline) {
		var got corev1.Pod
		// Uncached read: the informer cache lags on a loaded cluster and returned
		// stale probe-Pod state (see Reconciler.APIReader). This Get respects ctx
		// (unlike the cache reader), so the select below is belt-and-suspenders.
		if err := r.probeReader().Get(ctx, client.ObjectKeyFromObject(pod), &got); err != nil {
			if apierrors.IsNotFound(err) {
				if sawPod {
					logger.Info("sidecartoolbox: probe pod vanished before becoming ready",
						"toolbox", cr.Name, "pod", podName)
					return ProbeResult{}, errors.New("probe pod vanished before becoming ready")
				}
				// Not visible YET — the cache lags right after the Create. Keep
				// polling to the deadline rather than mis-reporting it as vanished;
				// bailing here failed every probe on a loaded cluster (read-after-
				// write race). If it truly never appears, the deadline below reports
				// "did not become Ready in time".
				select {
				case <-ctx.Done():
					return ProbeResult{}, fmt.Errorf("probe wait cancelled: %w", ctx.Err())
				case <-time.After(time.Second):
				}
				continue
			}
			logger.Info("sidecartoolbox: failed to read probe pod",
				"toolbox", cr.Name, "pod", podName, "err", err.Error())
			return ProbeResult{}, errors.New("failed to read reachability probe pod from the API server")
		}
		sawPod = true
		ready := false
		for _, c := range got.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if ready && got.Status.PodIP != "" {
			// The probe target is a Pod IP assigned by Kubernetes
			// (operator-controlled), not an LLM/user-supplied URL, so the safehttp
			// SSRF guard (which blocks private/pod IP ranges) must not apply.
			// probeURL carries transport.path: a toolbox declaring a custom path
			// is served there, not at "/", and probing "/" reports it unreachable.
			url := probeURL(got.Status.PodIP, cr)
			tools, err := r.dialAndList(ctx, url)
			if err == nil {
				return ProbeResult{Tools: tools, Digest: digestFromProbePod(&got)}, nil
			}
			// Fresh-pod window (see lastDialErr) — log the volatile detail and fall
			// through to the pacing select to retry within the deadline.
			lastDialErr = err
			logger.Info("sidecartoolbox: probe MCP dial failed; retrying within deadline",
				"toolbox", cr.Name, "url", url, "err", err.Error())
		}
		// Pace the poll and stay cancellable. The Get above now goes through the
		// uncached APIReader (which DOES honor ctx), but this select also paces
		// the loop to ~1/s and guarantees a never-Ready toolbox cannot pin the
		// sole reconcile worker (MaxConcurrentReconciles defaults to 1) past a
		// cancellation or manager shutdown.
		select {
		case <-ctx.Done():
			return ProbeResult{}, fmt.Errorf("probe wait cancelled: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if lastDialErr != nil {
		// Ready, but the MCP endpoint never accepted a connection within the
		// deadline. Stable message; lastDialErr was logged each attempt above.
		return ProbeResult{}, errors.New("probe pod became Ready but its MCP endpoint did not become reachable in time")
	}

	// One last read before the deferred delete above reaps the Pod: if the
	// probe container already terminated (a fail-closed image fatalling on a
	// missing/invalid AP_SIDECAR_CONFIG or upstream credential env var), its
	// exit message — copied into Pod status because of the
	// TerminationMessagePolicy set in buildProbePod — names the actual cause,
	// and probePhase writes this string verbatim as the Reachable=False
	// message. Without it the condition reads only "did not become Ready in
	// time", which is true and tells an operator nothing to act on.
	//
	// It is safe HERE and the pod name and IP are not: a termination message
	// is a pure function of the failure class — the same bytes every reconcile
	// — whereas a timestamped pod name or a freshly-assigned IP differs on
	// every pass, so embedding those rewrites the condition each time and the
	// controller's own status watch re-fires, minting a probe pod every couple
	// of seconds. Keep that distinction when editing this message.
	msg := "probe pod did not become Ready in time"
	var final corev1.Pod
	if getErr := r.Client.Get(ctx, client.ObjectKeyFromObject(pod), &final); getErr == nil {
		if tm := probeContainerFailureMessage(&final); tm != "" {
			msg = fmt.Sprintf("%s: container %q terminated: %s", msg, probeContainerName, tm)
			logger.Info("sidecartoolbox: probe pod container terminated with a message",
				"toolbox", cr.Name, "pod", pod.Name, "namespace", pod.Namespace, "message", tm)
		}
	}
	return ProbeResult{}, errors.New(msg)
}

// probeContainerFailureMessage returns the probe container's terminated exit
// message, if any. Checked via State.Terminated first — a Pod with
// RestartPolicy=Never that exits goes straight there and is never restarted,
// so LastTerminationState never populates in practice — with
// LastTerminationState.Terminated checked too as a defensive fallback.
func probeContainerFailureMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != probeContainerName {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			if msg := strings.TrimSpace(t.Message); msg != "" {
				return msg
			}
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			if msg := strings.TrimSpace(t.Message); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// digestFromProbePod extracts the kubelet-resolved image digest from the
// probe pod's ContainerStatuses. It looks for the container named
// probeContainerName and returns the digest via imagepin.DigestFromImageID.
// Returns "" when the container status is absent or carries no digest.
// This is a pure function to enable unit testing without a live API server.
func digestFromProbePod(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == probeContainerName {
			return imagepin.DigestFromImageID(cs.ImageID)
		}
	}
	return ""
}

// computeImagePin derives the new PinRecord (and whether drift was detected)
// from the declared image ref, the kubelet-resolved digest, and the prior
// status.pin baseline. It is a pure function — no API calls, no side effects —
// designed to be table-tested independently of the reconciler loop.
//
// Return values:
//   - rec: the PinRecord to write into status.pin (nil when no update needed
//     because the image kind kind reports an error for the declared ref).
//   - drifted: true iff the resolved digest diverged from the baseline.
//   - summary: human-readable drift message when drifted is true, or the
//     accept-hint message; empty when healthy.
//   - err: non-nil when image.ParseRef fails for the declared ref (surfaces as
//     PinVerifyFailed in the controller).
func computeImagePin(declaredRef, resolvedDigest string, prev *spiceboxv1alpha1.PinRecord) (
	rec *spiceboxv1alpha1.PinRecord, drifted bool, summary string, err error,
) {
	ik := &imagepin.Kind{}
	ref, parseErr := ik.ParseRef(declaredRef)
	if parseErr != nil {
		return nil, false, "", parseErr
	}

	// Determine pin version (tag) and baseline digest from the declared ref.
	_, tag, declaredDigest := imagepin.SplitRef(declaredRef)

	// Frozen declared ref: the baseline IS the declared digest. Compare
	// immediately — no TOFU needed.
	if ref.Strength == pinning.StrengthFrozen {
		baseline := declaredDigest
		if resolvedDigest != "" && resolvedDigest != baseline {
			msg := fmt.Sprintf(
				"image digest drifted from frozen declared ref: declared %s, resolved %s; "+
					"pin the source image by digest (@sha256:…) to assert, or accept the new digest via refreeze (Plan 4)",
				baseline, resolvedDigest)
			return nil, true, msg, nil
		}
		// Match (or no resolved digest available): record/refresh the baseline.
		now := metav1.Now()
		rec = &spiceboxv1alpha1.PinRecord{
			Kind:       imagepin.KindName,
			Strength:   string(ref.Strength),
			Digest:     baseline,
			Version:    tag, // empty for by-digest refs — no tag component
			ObservedAt: &now,
			Details:    map[string]string{"declaredRef": declaredRef},
		}
		// Preserve ObservedAt when the baseline hasn't changed.
		if prev != nil && prev.Digest == baseline {
			rec.ObservedAt = prev.ObservedAt
		}
		return rec, false, "", nil
	}

	// Named/unpinned: TOFU. Compare resolved digest against prior baseline.
	if resolvedDigest == "" {
		// No digest available (probe didn't populate it). Nothing to record yet.
		return nil, false, "", nil
	}

	// Declared-ref change = new identity, not content drift. Detect this by
	// comparing the stored declaredRef detail. Older records that predate this
	// field fall back to comparing the Version (tag) field; if both are absent
	// the prior record is treated as a fresh baseline (safe: first-observation
	// path below will TOFU the new digest).
	prevDeclaredRef := ""
	prevVersion := ""
	if prev != nil {
		prevDeclaredRef = prev.Details["declaredRef"]
		prevVersion = prev.Version
	}
	declaredRefChanged := prev != nil && prev.Digest != "" &&
		((prevDeclaredRef != "" && prevDeclaredRef != declaredRef) ||
			(prevDeclaredRef == "" && prevVersion != "" && prevVersion != tag))

	if !declaredRefChanged && prev != nil && prev.Digest != "" && resolvedDigest != prev.Digest {
		// Same declared ref but digest changed — drift detected.
		msg := fmt.Sprintf(
			"image tag %q now resolves to a different digest: baseline %s, resolved %s; "+
				"pin the source image by digest (@sha256:…) to assert, or accept the new digest via refreeze (Plan 4)",
			declaredRef, prev.Digest, resolvedDigest)
		return nil, true, msg, nil
	}

	// First observation (no prior record), or digest unchanged, or declared ref
	// changed (tag bump) — restamp the baseline with the new digest and
	// ObservedAt. A tag bump is a deliberate operator action, not drift.
	now := metav1.Now()
	rec = &spiceboxv1alpha1.PinRecord{
		Kind:       imagepin.KindName,
		Strength:   string(ref.Strength),
		Digest:     resolvedDigest,
		Version:    tag, // record the tag verbatim per forward-flag 1
		ObservedAt: &now,
		Details:    map[string]string{"declaredRef": declaredRef},
	}
	// Preserve ObservedAt when the digest is unchanged and the declared ref has
	// not changed (i.e. this is a true no-op reconcile, not a tag bump).
	if !declaredRefChanged && prev != nil && prev.Digest == resolvedDigest {
		rec.ObservedAt = prev.ObservedAt
	}
	return rec, false, "", nil
}

func (r *Reconciler) buildProbePod(cr *spiceboxv1alpha1.SidecarToolbox, name string) *corev1.Pod {
	c := corev1.Container{
		Name: probeContainerName,
		// AP_PROBE_MODE opts the ap-workshop image into declaration-only: it
		// serves tools/list (so this reachability probe passes) but refuses
		// every tool call, since a probe holds no workshop identity and never
		// will. A first-party fail-closed image reads it; images that do not
		// simply ignore an unknown env.
		Env: []corev1.EnvVar{
			{Name: "MCP_PORT", Value: fmt.Sprintf("%d", probePort)},
			{Name: "AP_PROBE_MODE", Value: "1"},
		},
		Ports: []corev1.ContainerPort{{ContainerPort: probePort}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: pickHealthPath(cr),
					Port: intstr.FromInt(probePort),
				},
			},
		},
		// Make a fail-closed image's real startup error readable off the Pod:
		// the kubelet copies the container's log tail into the terminated
		// status's Message on any non-zero exit, which is what lets a
		// never-Ready probe name its actual cause instead of just a timeout
		// (see probeContainerFailureMessage). Mirrors cosidecar.BuildContainer's
		// identical setting for the runtime sidecar container.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	// The probe must boot the image the way a session will: cosidecar.go
	// delivers spec.config the same way (as AP_SIDECAR_CONFIG) for the runtime
	// sidecar container. Without it, an admitted adapter's admission probe
	// never sees the config its own image requires and never becomes Ready.
	if cr.Spec.Config != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "AP_SIDECAR_CONFIG", Value: cr.Spec.Config})
	}
	// A placeholder credential value: the probe only exercises the MCP
	// surface (tools/list) — no tool is ever CALLED — so this placeholder
	// never reaches any upstream. The real credential is per-session and
	// does not exist yet at admission time (the per-session Secret isn't
	// created until pod-create). Without a value here, a fail-closed image
	// (e.g. ap-api-adapter) refuses to boot on a missing credential env var
	// and the probe times out with no cause; an image that ignores the var
	// entirely is unaffected either way.
	if ev := cr.Spec.UpstreamAuth.EnvVar; ev != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: ev, Value: "admission-probe-placeholder"})
	}
	var volumes []corev1.Volume
	switch {
	case cr.Spec.Source.Image != "":
		c.Image = cr.Spec.Source.Image
	case cr.Spec.Source.Inline != nil:
		c.Image = cr.Spec.Source.Inline.BaseImage
		c.Command = cr.Spec.Source.Inline.Entrypoint
		c.VolumeMounts = []corev1.VolumeMount{{Name: "src", MountPath: "/app", ReadOnly: true}}
		volumes = append(volumes, corev1.Volume{
			Name: "src",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cr.Spec.Source.Inline.Script.ConfigMapRef.Name},
					Items: []corev1.KeyToPath{{
						Key:  cr.Spec.Source.Inline.Script.ConfigMapRef.Key,
						Path: cr.Spec.Source.Inline.Script.ConfigMapRef.Key,
					}},
				},
			},
		})
	}
	ttl := probeTimeoutSeconds(cr)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cr.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:               "SidecarToolbox",
				Name:               cr.Name,
				UID:                cr.UID,
				Controller:         pointerTrue(),
				BlockOwnerDeletion: pointerTrue(),
			}},
			Labels: map[string]string{probePodLabelKey: cr.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			Containers:            []corev1.Container{c},
			Volumes:               volumes,
			ActiveDeadlineSeconds: &ttl,
		},
	}
}

// probeURL builds the tools/list target for a Ready probe Pod at podIP.
// cr.Spec.Transport.Path is the same source of truth the runtime sidecar's
// own reach paths derive from (see sidecartoolboxsynth.EndpointPathOf's doc
// comment) — both first-party sidecar images serve MCP only at "/mcp",
// nothing at "/", so a probe URL without this suffix 404s against the
// ServeMux before tools/list is ever reached. This is a pure function to
// enable unit testing the URL shape without a live Pod.
func probeURL(podIP string, cr *spiceboxv1alpha1.SidecarToolbox) string {
	return fmt.Sprintf("http://%s:%d%s", podIP, probePort, sidecartoolboxsynth.EndpointPathOf(cr.Spec.Transport.Path))
}

func pickHealthPath(cr *spiceboxv1alpha1.SidecarToolbox) string {
	if p := cr.Spec.Transport.Healthcheck.Path; p != "" {
		return p
	}
	return "/healthz"
}

func pointerTrue() *bool { v := true; return &v }

// sortNames is a small helper used by the controller to compare allowlist
// against observed.
func sortNames(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// probePodLabelKey is the probe pod's one selector label (value = the CR
// name), stamped by buildProbePod since the egress-external work: the
// operator's own egress allowlist already matches it by KEY to dial probe
// pods on :8080 (config/networkpolicy/egress-external.yaml, guarded by
// TestNetworkPolicyAllowsOperatorToReachSidecarProbe). That was HALF the data
// path. Under a namespace default-deny floor (agentprimitives-system ships
// one) the probe pod's INGRESS is also denied, and nothing admitted it — the
// operator's dial dropped for the whole deadline while the kubelet's
// readiness probe, running from the node's own netns, kept reporting Ready.
// buildProbeNetworkPolicy below is the ingress half, selecting this same
// label.
const probePodLabelKey = "agentprimitives.authzed.com/sidecartoolbox-probe"

// ProbeNetpolConfig carries the operator-level inputs for admission-probe
// NetworkPolicy stamping. Zero value = disabled — the same opt-in shape as
// agentsession's NetpolConfig, keyed off the same --session-network-policies
// flag by the operator binary; tests and the in-process e2e harness construct
// Reconcilers without it.
type ProbeNetpolConfig struct {
	Enabled bool
	// OperatorNamespace is where the operator pod lives — the ONLY ingress
	// peer a probe pod needs.
	OperatorNamespace string
}

// probeNetworkPolicyName names the per-toolbox admission-probe policy.
func probeNetworkPolicyName(cr *spiceboxv1alpha1.SidecarToolbox) string {
	return "probe-" + cr.Name + "-netpol"
}

// buildProbeNetworkPolicy returns the ingress-only policy admitting exactly
// the operator to this toolbox's probe pods on probePort. Owner-referenced to
// the toolbox: one standing object per toolbox, converged on every probe,
// reaped with the CR — never per-pod, so repeated probes do not churn
// policies.
func buildProbeNetworkPolicy(cr *spiceboxv1alpha1.SidecarToolbox, operatorNamespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      probeNetworkPolicyName(cr),
			Namespace: cr.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:               "SidecarToolbox",
				Name:               cr.Name,
				UID:                cr.UID,
				Controller:         pointerTrue(),
				BlockOwnerDeletion: pointerTrue(),
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{probePodLabelKey: cr.Name}},
			// Ingress-only ON PURPOSE: this policy must not change what a
			// probed image may dial out; egress posture stays whatever the
			// namespace already enforces.
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{netpolrule.ControlPlanePeer(operatorNamespace, "spicebox-operator")},
				Ports: []networkingv1.NetworkPolicyPort{netpolrule.Port(corev1.ProtocolTCP, probePort)},
			}},
		},
	}
}

// ensureProbeNetpol converges the probe policy before any probe pod exists.
// Update-on-exists rather than create-only: a shape change in a new operator
// version must land on clusters that already hold the old object.
func (r *Reconciler) ensureProbeNetpol(ctx context.Context, cr *spiceboxv1alpha1.SidecarToolbox) error {
	want := buildProbeNetworkPolicy(cr, r.ProbeNetpol.OperatorNamespace)
	var have networkingv1.NetworkPolicy
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), &have)
	switch {
	case apierrors.IsNotFound(err):
		return r.Client.Create(ctx, want)
	case err != nil:
		return err
	default:
		have.Spec = want.Spec
		have.OwnerReferences = want.OwnerReferences
		return r.Client.Update(ctx, &have)
	}
}
