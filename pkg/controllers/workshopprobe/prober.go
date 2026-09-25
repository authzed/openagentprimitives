// Package workshopprobe — prober.go: the seam that actually RUNS a probe.
// podbuild.go builds the pod/NetworkPolicy objects (no side effects, no
// cluster calls); this file is the other half — the controller-facing
// Prober that applies those objects, polls the pod to a terminal state,
// collects its result (a tools/list response or --help text), captures the
// kubelet-resolved image digest, and deletes the pod. It is also where
// validateProbeImage (podbuild.go) goes from a pure function nobody calls
// into an enforced gate: probeWith refuses a probe image before creating
// anything for it.
package workshopprobe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/platform/kube"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// ProbeResult bundles everything a probe run can produce. Exactly one of
// Tools (image/script mode) or HelpText (cliHelp mode) is populated on
// success; PodFailure is populated instead when the probe target itself was
// refused or never became usable — that is a RESULT of running the probe,
// not a controller error, so the caller (the WorkshopProbe controller, a
// later task) writes it to status rather than requeuing on it.
// ResolvedDigest is best-effort: it is populated whenever the kubelet
// reported one, on both the success and PodFailure paths, and left empty
// when it could not be resolved — that absence is never itself an error.
type ProbeResult struct {
	Tools          []spiceboxv1alpha1.WorkshopProbedTool
	HelpText       string
	ResolvedDigest string
	PodFailure     string
}

// Prober runs one WorkshopProbe's discriminated target and reports what it
// found. Interface + one implementation (PodProber) so the controller
// (a later task) can inject a fake in its own unit/integration tests
// without a live pod ever running there.
type Prober interface {
	Probe(ctx context.Context, wp *spiceboxv1alpha1.WorkshopProbe) (ProbeResult, error)
}

var _ Prober = (*PodProber)(nil)

// PodProber is the real Prober: it applies podbuild.go's hardened pod +
// NetworkPolicy, polls the pod to a terminal state, and collects the
// result.
type PodProber struct {
	Client client.Client
	// OperatorNamespace is passed through to BuildProbePod/BuildProbeNetworkPolicy
	// — the namespace the probe's ingress-allowed peer (the operator itself)
	// runs in.
	OperatorNamespace string
	// TrustedImageRegistry is this cluster's own first-party image registry,
	// read by the controller (a later task) the same way plan 2's admission
	// webhook reads it (see internal/cmd/operator/main.go's
	// trustedImageRegistryFrom). Probe wires it straight through to
	// probeWith/validateProbeImage; empty disables the first-party-registry
	// refusal (every local/desktop install).
	TrustedImageRegistry string
	// ListTools issues tools/list against a probe pod's MCP endpoint. Nil
	// defaults to a probe.Client-backed call — see listToolsFunc. Tests
	// substitute this so no real pod/MCP server is needed.
	ListTools func(ctx context.Context, url string) ([]probe.Tool, error)

	// podLogs reads a terminated cliHelp-mode probe pod's container logs.
	// Nil defaults to readPodLogs (a real client-go Clientset read) — see
	// podLogsFunc. Unexported: only this package's own tests need to
	// substitute it, since envtest has no kubelet and the fake client has no
	// log endpoint to serve from.
	podLogs func(ctx context.Context, namespace, podName, container string) (string, error)
	// pollInterval is the sleep between polls of the probe pod's status.
	// Zero defaults to one second (waitForProbePod). Unexported: only this
	// package's own tests shrink it, to keep a status-flip-in-the-background
	// test fast without changing the poll loop's real-world cadence.
	pollInterval time.Duration
}

// Probe implements Prober by delegating to probeWith with the configured
// trusted registry — split out so a test can inject a trusted registry
// directly without touching PodProber's exported field.
func (p *PodProber) Probe(ctx context.Context, wp *spiceboxv1alpha1.WorkshopProbe) (ProbeResult, error) {
	return p.probeWith(ctx, wp, p.TrustedImageRegistry)
}

// probeWith runs wp's discriminated probe target end to end: refuse an
// untrusted/malformed image before creating anything; stage the Script-mode
// ConfigMap and apply the NetworkPolicy; create the pod; poll it to a
// terminal state; collect tools/list or the container's logs; capture the
// resolved digest; delete the pod.
//
// A refused image, a pod that never becomes usable before its deadline, a
// container that fails, or a tools/list/log-read error are all RESULTS
// (ProbeResult.PodFailure, err == nil) — the caller writes them to
// WorkshopProbe.status rather than treating them as a reconcile error. Only
// an infrastructure fault (the pod vanishing, an API error, ctx
// cancellation, a failure to even build/apply the pod/NetworkPolicy/
// ConfigMap) returns a non-nil error.
func (p *PodProber) probeWith(ctx context.Context, wp *spiceboxv1alpha1.WorkshopProbe, trustedImageRegistry string) (ProbeResult, error) {
	image, err := probeImageRef(wp)
	if err != nil {
		return ProbeResult{}, err
	}
	if verr := validateProbeImage(image, trustedImageRegistry); verr != nil {
		// A refused image is a probe RESULT, not a controller error — and no
		// pod, NetworkPolicy, or ConfigMap is created for it.
		return ProbeResult{PodFailure: verr.Error()}, nil
	}

	if wp.Spec.Script != nil {
		if err := p.ensureScriptConfigMap(ctx, wp); err != nil {
			return ProbeResult{}, fmt.Errorf("stage probe script configmap for %s/%s: %w", wp.Namespace, wp.Name, err)
		}
	}

	np := BuildProbeNetworkPolicy(wp, p.OperatorNamespace)
	if err := p.Client.Patch(ctx, np, client.Apply, client.ForceOwnership, client.FieldOwner(probePodFieldOwner)); err != nil {
		return ProbeResult{}, fmt.Errorf("apply probe NetworkPolicy %q: %w", np.Name, err)
	}

	pod, err := BuildProbePod(wp, p.OperatorNamespace)
	if err != nil {
		return ProbeResult{}, err
	}
	if err := p.Client.Create(ctx, pod); err != nil {
		return ProbeResult{}, fmt.Errorf("create probe pod %q: %w", pod.Name, err)
	}
	defer func() {
		if delErr := p.Client.Delete(context.Background(), pod); delErr != nil && !apierrors.IsNotFound(delErr) {
			log.FromContext(ctx).Info("workshopprobe: failed to delete probe pod",
				"pod", pod.Name, "namespace", pod.Namespace, "err", delErr.Error())
		}
	}()

	cliHelp := wp.Spec.CliHelp != nil
	got, podFailure, err := p.waitForProbePod(ctx, client.ObjectKeyFromObject(pod), cliHelp, probeDeadline(wp))
	if err != nil {
		return ProbeResult{}, err
	}
	digest := digestFromProbePod(got)
	if podFailure != "" {
		return ProbeResult{PodFailure: podFailure, ResolvedDigest: digest}, nil
	}

	if cliHelp {
		logs, logErr := p.podLogsFunc()(ctx, got.Namespace, got.Name, probeContainerName)
		if logErr != nil {
			return ProbeResult{PodFailure: fmt.Sprintf("reading probe pod logs: %v", logErr), ResolvedDigest: digest}, nil
		}
		return ProbeResult{HelpText: logs, ResolvedDigest: digest}, nil
	}

	url := fmt.Sprintf("http://%s:%d", got.Status.PodIP, ProbeMCPPort)
	tools, toolErr := p.listToolsFunc()(ctx, url)
	if toolErr != nil {
		return ProbeResult{PodFailure: fmt.Sprintf("tools/list: %v", toolErr), ResolvedDigest: digest}, nil
	}
	return ProbeResult{Tools: mapProbedTools(tools), ResolvedDigest: digest}, nil
}

// probeImageRef returns the image ref of whichever spec variant is set,
// mirroring BuildProbePod's own discriminator switch. A CEL rule on
// WorkshopProbe.Spec (`workshopprobe_types.go`) enforces "exactly one of
// image/script/cliHelp" at admission (no webhook), so a CR should never
// reach here with none set, but this still fails closed rather than
// silently treating a malformed spec as an empty (i.e. untrusted-registry-
// exempt) ref.
func probeImageRef(wp *spiceboxv1alpha1.WorkshopProbe) (string, error) {
	switch {
	case wp.Spec.Image != "":
		return wp.Spec.Image, nil
	case wp.Spec.Script != nil:
		return wp.Spec.Script.BaseImage, nil
	case wp.Spec.CliHelp != nil:
		return wp.Spec.CliHelp.Image, nil
	default:
		return "", fmt.Errorf("workshopprobe %s/%s: spec has none of image, script, cliHelp set", wp.Namespace, wp.Name)
	}
}

// probeDeadline mirrors BuildProbePod's own ActiveDeadlineSeconds
// derivation so the poll loop's wall-clock budget always matches the
// budget the pod itself is killed at — a smaller poll deadline would report
// "did not become ready" for a pod the operator's own ActiveDeadlineSeconds
// hadn't even killed yet.
func probeDeadline(wp *spiceboxv1alpha1.WorkshopProbe) time.Time {
	seconds := wp.Spec.TimeoutSeconds
	if seconds <= 0 {
		seconds = defaultProbeTimeoutSeconds
	}
	return time.Now().Add(time.Duration(seconds) * time.Second)
}

// ensureScriptConfigMap SSA-applies the Script-mode ConfigMap
// BuildProbePod's Script-mode pod mounts by name — a pure builder has no
// client to create it with, so the Prober owns it. Owner-refed to wp so it
// is reaped with the CR/namespace like the pod and NetworkPolicy. Applied
// BEFORE the pod: the pod's ConfigMap volume only resolves once the
// ConfigMap exists.
//
// SSA-applied (client.Apply), not create-then-get-then-update: a retry that
// hit Create's AlreadyExists used to read the ConfigMap back through
// p.Client — the SAME cache-backed client every other Get in this package
// uses — and this operator's manager cache filters the ConfigMap informer
// to adoptguard.AdoptedLabel-carrying objects
// (internal/cmd/operator/main.go's Cache.Options.ByObject); this ConfigMap
// carries no such label, so that cached Get would spuriously report
// NotFound for a ConfigMap that genuinely exists, breaking a retried
// Script-mode probe on a real cluster. SSA needs no read at all: a
// byte-identical re-apply (the same script content — WorkshopProbeSpec is
// immutable after creation, so it can never actually differ) is a true
// no-op, applied blind.
func (p *PodProber) ensureScriptConfigMap(ctx context.Context, wp *spiceboxv1alpha1.WorkshopProbe) error {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            ProbeScriptConfigMapName(wp),
			Namespace:       wp.Namespace,
			OwnerReferences: []metav1.OwnerReference{probeOwnerRef(wp)},
		},
		Data: map[string]string{ProbeScriptConfigMapKey: wp.Spec.Script.Script},
	}
	if err := p.Client.Patch(ctx, cm, client.Apply, client.ForceOwnership, client.FieldOwner(probePodFieldOwner)); err != nil {
		return fmt.Errorf("apply probe script configmap %q: %w", cm.Name, err)
	}
	return nil
}

// waitForProbePod polls the probe pod named by key until probePodStatus
// says it is usable or has definitively failed, or deadline passes.
//
// Returns (pod, "", nil) when usable; (pod, failureReason, nil) when the
// pod reached a terminal failure state OR the deadline passed with neither
// — both are probe RESULTS, so err stays nil and the caller reports
// PodFailure rather than erroring the reconcile. Only an infrastructure
// fault (the pod vanishing, a Get error, ctx cancellation) returns a
// non-nil err. pod is returned whenever one was successfully fetched — even
// alongside a non-empty failureReason — so the caller can still attempt
// digest capture from its ContainerStatuses.
func (p *PodProber) waitForProbePod(ctx context.Context, key client.ObjectKey, cliHelp bool, deadline time.Time) (*corev1.Pod, string, error) {
	interval := p.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		var got corev1.Pod
		if err := p.Client.Get(ctx, key, &got); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "", fmt.Errorf("probe pod %q vanished before completion", key.Name)
			}
			return nil, "", fmt.Errorf("get probe pod %q: %w", key.Name, err)
		}
		last = &got
		if usable, failure := probePodStatus(&got, cliHelp); usable {
			return &got, "", nil
		} else if failure != "" {
			return &got, failure, nil
		}
		// Wait on ctx too. Client.Get above is informer-cache served in
		// production (controller-runtime's CacheReader ignores its ctx
		// entirely), so this select is the loop's ONLY cancellation point —
		// mirrors pkg/controllers/sidecartoolbox/probe.go's probeOnce.
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("probe pod %q wait cancelled: %w", key.Name, ctx.Err())
		case <-time.After(interval):
		}
	}
	return last, fmt.Sprintf("probe pod %q did not become ready before the configured timeout", key.Name), nil
}

// probePodStatus classifies a probe pod's CURRENT status into "usable"
// (proceed: call tools/list for image/script mode, read logs for cliHelp
// mode), a definitive failure (a reason worth reporting as PodFailure
// immediately, without waiting out the rest of the deadline), or neither
// (keep polling). It is a pure function of pod.Status so this
// classification is unit-testable without a real kubelet driving state
// transitions.
//
// image/script mode runs a long-lived MCP server: usable means
// PodReady==true with a PodIP; the container terminating at all, before
// ever being reachable, is itself the failure. cliHelp mode runs
// `<binary> --help` to completion: usable means the container has
// terminated (any exit code — many CLIs exit non-zero on --help), and only
// a pull/create failure (never simply "still running") counts as failed.
func probePodStatus(pod *corev1.Pod, cliHelp bool) (usable bool, failure string) {
	var cs *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == probeContainerName {
			cs = &pod.Status.ContainerStatuses[i]
			break
		}
	}

	if cliHelp {
		switch {
		case cs != nil && cs.State.Terminated != nil:
			return true, ""
		case cs != nil && cs.State.Waiting != nil && isProbeCrashReason(cs.State.Waiting.Reason):
			return false, fmt.Sprintf("probe pod %s/%s container %s: %s",
				pod.Namespace, pod.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
		default:
			return false, ""
		}
	}

	switch {
	case cs != nil && cs.State.Terminated != nil:
		return false, fmt.Sprintf("probe pod %s/%s: mcp server container exited before it was reachable (exit code %d): %s",
			pod.Namespace, pod.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
	case cs != nil && cs.State.Waiting != nil && isProbeCrashReason(cs.State.Waiting.Reason):
		return false, fmt.Sprintf("probe pod %s/%s container %s: %s",
			pod.Namespace, pod.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && pod.Status.PodIP != "" {
			return true, ""
		}
	}
	return false, ""
}

// isProbeCrashReason reports whether a container Waiting reason is a
// terminal failure worth reporting immediately rather than continuing to
// poll out the rest of the deadline (an unresolvable image, a malformed
// container config) — as opposed to an ordinary in-progress reason
// (ContainerCreating, PodInitializing) that still might resolve.
func isProbeCrashReason(reason string) bool {
	switch reason {
	case "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff",
		"CreateContainerConfigError", "CreateContainerError",
		"InvalidImageName", "RunContainerError":
		return true
	}
	return false
}

// digestFromProbePod extracts the kubelet-resolved image digest from the
// probe pod's ContainerStatuses. Returns "" when pod is nil, the container
// status is absent, or it carries no digest — never an error: an
// unresolved digest is not itself a probe failure.
func digestFromProbePod(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == probeContainerName {
			return imagepin.DigestFromImageID(cs.ImageID)
		}
	}
	return ""
}

// listToolsFunc returns p.ListTools when set, else a probe.Client-backed
// default. The default explicitly sets HTTP to an unguarded client: the
// probe target is a Kubernetes-assigned Pod IP (operator-controlled, not an
// LLM/user-supplied URL), and probe.Client's nil-HTTP default is the
// SSRF-guarded safehttp.Client(), which correctly refuses private/pod-IP
// destinations — refusing our own legitimate pod-IP call along with them.
// Mirrors pkg/controllers/sidecartoolbox/probe.go's probeOnce, which
// documents the identical reasoning at its own probe.Client construction.
func (p *PodProber) listToolsFunc() func(ctx context.Context, url string) ([]probe.Tool, error) {
	if p.ListTools != nil {
		return p.ListTools
	}
	return func(ctx context.Context, url string) ([]probe.Tool, error) {
		return (&probe.Client{HTTP: &http.Client{}, URL: url, Timeout: 20 * time.Second}).ListTools(ctx, "", "")
	}
}

// podLogsFunc returns p.podLogs when set (tests only), else readPodLogs.
func (p *PodProber) podLogsFunc() func(ctx context.Context, namespace, podName, container string) (string, error) {
	if p.podLogs != nil {
		return p.podLogs
	}
	return readPodLogs
}

// readPodLogs is the real (non-test) pod-log reader: it builds its own
// client-go Clientset from the ambient REST config (in-cluster in
// production, kubeconfig for local dev — see pkg/platform/kube.RestConfig)
// rather than taking one as a PodProber field, since building one is cheap
// and stateless (the same judgment internal/cmd/operator/main.go makes for
// its own on-demand kubernetes.NewForConfig calls) and a probe's log read
// is a rare, per-probe operation, not a hot path.
func readPodLogs(ctx context.Context, namespace, podName, container string) (string, error) {
	cfg, err := kube.RestConfig()
	if err != nil {
		return "", fmt.Errorf("build rest config for probe pod logs: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("build clientset for probe pod logs: %w", err)
	}
	stream, err := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: container}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("open log stream for probe pod %s/%s: %w", namespace, podName, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("read log stream for probe pod %s/%s: %w", namespace, podName, err)
	}
	return string(data), nil
}

// mapProbedTools converts an MCP tools/list response into the CRD status
// shape. InputSchema is a json.RawMessage on the wire type; string(...) is
// its marshaled form (json.RawMessage.MarshalJSON returns itself verbatim),
// which is what a CRD status field (a plain string, not raw JSON) needs.
func mapProbedTools(in []probe.Tool) []spiceboxv1alpha1.WorkshopProbedTool {
	out := make([]spiceboxv1alpha1.WorkshopProbedTool, 0, len(in))
	for _, t := range in {
		schema := ""
		if len(t.InputSchema) > 0 {
			schema = string(t.InputSchema)
		}
		out = append(out, spiceboxv1alpha1.WorkshopProbedTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out
}
