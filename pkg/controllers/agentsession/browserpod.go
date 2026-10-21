// pkg/controllers/agentsession/browserpod.go
//
// Per-session browser pod: two containers (chrome, gate) from the first-party
// browser image (images/browser/Dockerfile), the policy ConfigMap the gate
// reloads, the per-session gate-token Secret it is injected from, and the
// NetworkPolicy that isolates the pod (netpol.go). Pure builders only --
// Task 8b wires the reconcile loop (create, readiness, status, reap/wake)
// around these.
package agentsession

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/tools/browsergate"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

const browserGatePort int32 = browsergate.DefaultGatePort // 9223
const browserDebugPort = "9222"
const browserProxyAddr = "127.0.0.1:3128"

// Pinned stable real-Chrome UA (spec §2.1); bump deliberately, never per-build:
const browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

const (
	// labelBrowserToolbox identifies the separate browser pod's toolbox ref --
	// the sibling of labelSidecarToolbox (sidecarpod.go) for this toolbox's
	// own per-session pod.
	labelBrowserToolbox = "agentprimitives.authzed.com/browsertoolbox"

	// browserImageUID is the ONE account images/browser/Dockerfile declares
	// (useradd --uid 65532 chrome). See BuildBrowserPod's chrome-container
	// comment for why this must be pinned per-container rather than inherited
	// from podspec.HardenedPodSecurityContext's RunAsUser:1000.
	browserImageUID = 65532

	// browserTokenSecretKey is the Secret data key the per-session gate token
	// is written under (Task 8b's mintBrowserToken) and the key the gate
	// container's APBROWSERGATE_TOKEN env reads back via secretKeyRef.
	browserTokenSecretKey = "token"
	// browserPolicyConfigMapKey is the ConfigMap data key the JSON PolicyDoc
	// is written under (Task 8b's ensureBrowserPolicyConfigMap), and the file
	// name it appears as under the gate's policy mount -- so
	// APBROWSERGATE_POLICY_PATH resolves to exactly this file.
	browserPolicyConfigMapKey = "policy.json"
	// browserPolicyMountPath is where the policy ConfigMap is mounted,
	// read-only, in the gate container only.
	browserPolicyMountPath = "/etc/browserpolicy"
	// browserProfileMountPath is Chrome's --user-data-dir: an emptyDir, so a
	// fresh (ephemeral) profile every pod boot -- matches
	// BrowserToolboxSpec.State's only Phase A value.
	browserProfileMountPath = "/home/chrome/profile"

	browserProfileVolume = "profile"
	browserTmpVolume     = "tmp"
	browserPolicyVolume  = "policy"
)

// BrowserPodName returns the deterministic name for a session's per-session
// browser pod for the given toolbox ref: "<session>-browser-<ref>". Mirrors
// SidecarPodName's plain concatenation (sidecarpod.go:66) -- neither builder
// truncates for a name that would exceed Kubernetes' 253-char object-name
// limit; there is no separate name-truncation helper in this package today.
func BrowserPodName(sess *spiceboxv1alpha1.AgentSession, ref string) string {
	return sess.Name + "-browser-" + ref
}

// BrowserTokenSecretName returns the per-session gate-token Secret's name:
// "<session>-browser-<ref>-token".
func BrowserTokenSecretName(sess *spiceboxv1alpha1.AgentSession, ref string) string {
	return BrowserPodName(sess, ref) + "-token"
}

// BrowserPolicyConfigMapName returns the per-session browser policy
// ConfigMap's name: "<session>-browser-<ref>-policy".
func BrowserPolicyConfigMapName(sess *spiceboxv1alpha1.AgentSession, ref string) string {
	return BrowserPodName(sess, ref) + "-policy"
}

// BrowserNetworkPolicyName returns the per-session browser NetworkPolicy's
// name: "<session>-browser-<ref>-netpol" -- mirrors SidecarNetworkPolicyName
// (netpol.go:328).
func BrowserNetworkPolicyName(sess *spiceboxv1alpha1.AgentSession, ref string) string {
	return BrowserPodName(sess, ref) + "-netpol"
}

// DeriveBrowserEffectiveSites maps spec.Access.Sites 1:1 into the resolved
// EffectiveSites list, Source "explicit", in order. Phase A has no other
// site source (identity-derived sites arrive in Phase B), so this is a
// straight carry-through -- except for host canonicalization, which is NOT
// optional: browserpolicy.Match (and Decide) assume every Site.Host they
// compare against is ALREADY canonical (lowercased, no trailing dot), and
// this is the one place that guarantee is established before the value
// reaches the gate's PolicyDoc (BuildBrowserPolicyDoc) or any runner-side
// pre-check.
//
// Pure and deterministic: same spec in, same slice out, on every call -- no
// clock, no randomness, no I/O. That purity is what makes the status write
// (ResolvedBrowserToolbox.EffectiveSites) idempotent across re-reconciles.
//
// A Host that fails CanonicalHost is DROPPED rather than carried through
// un-normalized or erroring the whole derivation. The CRD's Pattern marker on
// BrowserSite.Host (browsertoolbox_types.go) already forbids anything
// CanonicalHost would reject, so this path is believed unreachable in
// practice -- but failing closed costs nothing here: in allowlist mode a
// dropped entry only NARROWS the effective allowlist (strictly safer than
// including a malformed entry verbatim); in blocklist mode dropping an entry
// would instead WIDEN reachability for that one host, which is exactly why
// the CRD pattern exists to keep this branch unreachable in the first place,
// rather than relying on this fail-closed default alone.
func DeriveBrowserEffectiveSites(spec spiceboxv1alpha1.BrowserToolboxSpec) []spiceboxv1alpha1.BrowserEffectiveSite {
	sites := make([]spiceboxv1alpha1.BrowserEffectiveSite, 0, len(spec.Access.Sites))
	for _, s := range spec.Access.Sites {
		host, err := browserpolicy.CanonicalHost(s.Host)
		if err != nil {
			continue
		}
		sites = append(sites, spiceboxv1alpha1.BrowserEffectiveSite{
			Host:   host,
			Exact:  s.Exact,
			Source: "explicit",
		})
	}
	return sites
}

// BuildBrowserPolicyDoc converts a ResolvedBrowserToolbox's effective sites
// (plus its Access mode) into the browserpolicy.PolicyDoc JSON document the
// gate reloads from the per-session ConfigMap.
func BuildBrowserPolicyDoc(rt spiceboxv1alpha1.ResolvedBrowserToolbox) browserpolicy.PolicyDoc {
	sites := make([]browserpolicy.Site, 0, len(rt.EffectiveSites))
	for _, s := range rt.EffectiveSites {
		sites = append(sites, browserpolicy.Site{Host: s.Host, Exact: s.Exact})
	}
	return browserpolicy.PolicyDoc{Mode: rt.Spec.Access.Mode, Sites: sites}
}

// BuildBrowserPod builds the per-session browser pod: two containers from
// imageRef (images/browser/Dockerfile) sharing one pod network namespace --
// "chrome" (headless Chromium, egress forced through the gate's loopback
// forward proxy) and "gate" (browsergate.Gate: the policy-enforcing proxy
// Chrome's --proxy-server points at, plus the authenticated DevTools
// reverse-proxy the runner calls).
//
// sandbox decides whether chrome's command appends --no-sandbox (Sandboxed
// false -- every Phase A cloud.Strategy, see cloud.BrowserChromeSandbox's own
// doc comment) or instead relies on Chrome's native seccomp/userns sandbox
// (Sandboxed true; not yet reachable in Phase A).
//
// Security posture: the pod-level context is
// podspec.HardenedPodSecurityContext() UNMODIFIED (RunAsNonRoot, UID/GID/
// FSGroup 1000, seccomp RuntimeDefault). The GATE container inherits it
// unmodified -- fully hardened, no per-container relaxation, exactly like
// every other hardened container in this package. The CHROME container
// overrides RunAsUser/RunAsGroup to 65532: images/browser/Dockerfile's only
// account (useradd --uid 65532 chrome) owns /home/chrome, mode 0700 -- a
// container running as the pod-level default UID 1000 could not even
// traverse into that directory to reach the --user-data-dir mount point
// below it. ReadOnlyRootFilesystem, AllowPrivilegeEscalation=false and
// Capabilities Drop:[ALL] are NOT relaxed for chrome; two emptyDir volumes
// supply the writable paths it actually needs instead:
//
//   - "profile" at browserProfileMountPath (--user-data-dir): ephemeral by
//     construction -- a fresh profile every pod boot, matching
//     BrowserToolboxSpec.State's only Phase A value.
//   - "tmp" at /tmp: chromium's --disable-dev-shm-usage avoids the default
//     64MB /dev/shm tmpfs by writing shared-memory-backed temp files under
//     the OS temp dir instead, which defaults to /tmp -- the same
//     ReadOnlyRootFilesystem-needs-a-writable-/tmp pattern
//     cosidecar.BuildContainer and the runner/sandbox pods
//     (pkg/controllers/agentsession/podspec.go, pkg/platform/podspec) already
//     use, rather than redirecting TMPDIR or relaxing the flag.
//
// Both emptyDirs are writable by chrome's overridden UID via the pod-level
// FSGroup:1000 supplementary group kubelet adds to EVERY container
// regardless of that container's own RunAsUser/RunAsGroup -- so FSGroup
// itself needs no override.
func BuildBrowserPod(sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedBrowserToolbox, imageRef string, sandbox cloud.BrowserChromeSandbox) (*corev1.Pod, error) {
	if imageRef == "" {
		return nil, fmt.Errorf("build browser pod %q: empty image ref", rt.Ref)
	}

	chromeSecCtx := podspec.HardenedContainerSecurityContext()
	chromeSecCtx.RunAsUser = ptr.To(int64(browserImageUID))
	chromeSecCtx.RunAsGroup = ptr.To(int64(browserImageUID))

	chromeCmd := []string{
		"chromium",
		"--headless=new",
		"--remote-debugging-port=" + browserDebugPort,
		"--remote-debugging-address=127.0.0.1",
		"--proxy-server=" + browserProxyAddr,
		"--proxy-bypass-list=<-loopback>",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-dev-shm-usage",
		// Crashpad needs a writable database directory before it parses any
		// disable flag: without these three, the handler exits with
		// "--database is required" and chromium dies with SIGTRAP (exit 133)
		// under ReadOnlyRootFilesystem, because the image's /home/chrome is
		// read-only at runtime (found on a real kubelet; no suite runs the
		// real image under the real securityContext).
		"--disable-crash-reporter",
		"--disable-breakpad",
		"--crash-dumps-dir=/tmp",
		"--user-data-dir=" + browserProfileMountPath,
		"--user-agent=" + browserUserAgent,
		"about:blank",
	}
	if !sandbox.Sandboxed {
		chromeCmd = append(chromeCmd, "--no-sandbox")
	}

	chrome := corev1.Container{
		Name:            "chrome",
		Image:           imageRef,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         chromeCmd,
		// HOME points at the profile emptyDir: the image's passwd entry says
		// /home/chrome, which is read-only at runtime, and chromium writes
		// $HOME/.config and $HOME/.pki on boot.
		Env: []corev1.EnvVar{{Name: "HOME", Value: browserProfileMountPath}},
		SecurityContext: chromeSecCtx,
		VolumeMounts: []corev1.VolumeMount{
			{Name: browserProfileVolume, MountPath: browserProfileMountPath},
			{Name: browserTmpVolume, MountPath: "/tmp"},
		},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	gate := corev1.Container{
		Name:            "gate",
		Image:           imageRef,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"apbrowsergate"},
		Env: []corev1.EnvVar{
			{
				Name: "APBROWSERGATE_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: BrowserTokenSecretName(sess, rt.Ref)},
						Key:                  browserTokenSecretKey,
					},
				},
			},
			{Name: "APBROWSERGATE_POLICY_PATH", Value: browserPolicyMountPath + "/" + browserPolicyConfigMapKey},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: browserPolicyVolume, MountPath: browserPolicyMountPath, ReadOnly: true},
		},
		SecurityContext: podspec.HardenedContainerSecurityContext(),
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(browserGatePort),
				},
			},
			PeriodSeconds:    1,
			FailureThreshold: 30, // mirrors cosidecar.buildStartupProbe's default give-up window
		},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BrowserPodName(sess, rt.Ref),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
				labelBrowserToolbox:                        rt.Ref,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: corev1.PodSpec{
			// A long-running per-session server: restart on crash, mirroring
			// BuildSidecarPod's separate-pod rationale.
			RestartPolicy: corev1.RestartPolicyOnFailure,
			// The browser pod has no business holding an apiserver credential.
			AutomountServiceAccountToken: ptr.To(false),
			SecurityContext:              podspec.HardenedPodSecurityContext(),
			Containers:                   []corev1.Container{chrome, gate},
			Volumes: []corev1.Volume{
				{
					Name:         browserProfileVolume,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				{
					Name:         browserTmpVolume,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				{
					Name: browserPolicyVolume,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: BrowserPolicyConfigMapName(sess, rt.Ref)},
							Items: []corev1.KeyToPath{{
								Key:  browserPolicyConfigMapKey,
								Path: browserPolicyConfigMapKey,
							}},
						},
					},
				},
			},
		},
	}
	return pod, nil
}

// browserPodState is what one reconcile pass observed about a per-session
// browser pod. Mirrors sidecarPodState, minus the token-rotation Replacing case:
// the gate token is read-before-mint (mintBrowserToken), so it never rotates and
// the pod is never torn down for a token change. PodIP is set once the pod is
// Ready; Failure is non-nil only for a terminal (crashloop / unpullable) wedge.
type browserPodState struct {
	PodIP   string
	Failure *spiceboxv1alpha1.SidecarPodFailure
}

// mintBrowserToken resolves the per-session gate-token Secret and stamps
// rt.TokenHash from the value actually in hand. READ-BEFORE-MINT is the whole
// point: an existing Secret is reused verbatim, so a re-reconcile — and a
// reap/wake, where the pod is deleted but the Secret survives — recomputes the
// SAME hash and never rewrites the token. Only when the Secret is absent is a
// fresh 32-byte hex token minted and created (with the adoption label so the
// operator's later guarded reads find it in the label-filtered cache). A Create
// race is resolved by re-reading the winner's value.
func (r *Reconciler) mintBrowserToken(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, rt *spiceboxv1alpha1.ResolvedBrowserToolbox) error {
	name := BrowserTokenSecretName(sess, rt.Ref)
	key := types.NamespacedName{Namespace: sess.Namespace, Name: name}
	sec, err := r.getSecret(ctx, key)
	switch {
	case err == nil:
		// Existing token — reuse it unchanged.
	case errors.IsNotFound(err):
		tok, genErr := generateToken()
		if genErr != nil {
			return fmt.Errorf("generate browser gate token for %q: %w", rt.Ref, genErr)
		}
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: sess.Namespace,
				Labels: map[string]string{
					"agentprimitives.authzed.com/agentsession": sess.Name,
				},
				OwnerReferences: sessionOwnerRef(sess),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{browserTokenSecretKey: []byte(tok)},
		}
		// Enter the label-filtered cache + pass the SecretReader guard on later
		// reads, exactly like the memory-token Secret.
		adoptguard.WithAdoptedLabel(sec)
		if createErr := r.Client.Create(ctx, sec); createErr != nil {
			if !errors.IsAlreadyExists(createErr) {
				return fmt.Errorf("create browser gate-token Secret %q: %w", name, createErr)
			}
			// Raced with another writer: re-read so TokenHash reflects the token
			// that actually landed, not the one this pass generated and discarded.
			reread, getErr := r.getSecret(ctx, key)
			if getErr != nil {
				return fmt.Errorf("get browser gate-token Secret %q after create race: %w", name, getErr)
			}
			sec = reread
		}
	default:
		return fmt.Errorf("get browser gate-token Secret %q: %w", name, err)
	}
	rt.TokenHash = SecretTokenHash(sec.Data[browserTokenSecretKey])
	return nil
}

// ensureBrowserNetworkPolicy stamps (SSA) one per-session browser pod's
// NetworkPolicy. Called BEFORE the pod is created so an enforcing CNI never sees
// an unconstrained browser pod. BuildBrowserNetworkPolicy returns nil when
// per-session network policies are disabled, in which case this is a no-op
// (mirrors every other ensure*NetworkPolicy's disabled behavior).
func (r *Reconciler) ensureBrowserNetworkPolicy(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedBrowserToolbox) error {
	np := BuildBrowserNetworkPolicy(sess, rt, r.Netpol)
	if np == nil {
		return nil
	}
	if err := r.Client.Patch(ctx, np,
		client.Apply, client.ForceOwnership, client.FieldOwner(netpolFieldOwner),
	); err != nil {
		return fmt.Errorf("apply browser NetworkPolicy %q: %w", np.Name, err)
	}
	return nil
}

// ensureBrowserPolicyConfigMap writes the gate's policy document (the JSON the
// gate reloads) into the per-session ConfigMap. Idempotent and WRITE-IF-DIFFERENT:
// a byte-identical re-reconcile is a no-op, so the ConfigMap's resourceVersion —
// and the mounted policy the gate watches — never churns.
func (r *Reconciler) ensureBrowserPolicyConfigMap(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedBrowserToolbox) error {
	raw, err := json.Marshal(BuildBrowserPolicyDoc(rt))
	if err != nil {
		return fmt.Errorf("marshal browser policy for %q: %w", rt.Ref, err)
	}
	name := BrowserPolicyConfigMapName(sess, rt.Ref)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
				labelBrowserToolbox:                        rt.Ref,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Data: map[string]string{browserPolicyConfigMapKey: string(raw)},
	}
	adoptguard.WithAdoptedLabel(cm)
	if err := r.Client.Create(ctx, cm); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create browser policy ConfigMap %q: %w", name, err)
		}
		existing, getErr := r.getConfigMap(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: name})
		if getErr != nil {
			return fmt.Errorf("get browser policy ConfigMap %q: %w", name, getErr)
		}
		// Write-if-different: an unchanged document is left exactly as is.
		if existing.Data[browserPolicyConfigMapKey] == string(raw) {
			return nil
		}
		existing.Data = map[string]string{browserPolicyConfigMapKey: string(raw)}
		if updErr := r.Client.Update(ctx, existing); updErr != nil {
			return fmt.Errorf("update browser policy ConfigMap %q: %w", name, updErr)
		}
	}
	return nil
}

// reconcileBrowserPod idempotently creates the per-session browser pod and
// reflects its readiness. Returns the PodIP once the pod is Ready (the runner
// then reaches the gate at http://<PodIP>:<GatePort>), or a Failure when the pod
// is wedged in a terminal container state. All-zero means "created, still
// starting, check again next pass". Mirrors reconcileSidecarPod minus token
// rotation — see browserPodState.
func (r *Reconciler) reconcileBrowserPod(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedBrowserToolbox) (browserPodState, error) {
	desired, berr := BuildBrowserPod(sess, rt, r.BrowserImageRef, r.BrowserSandbox)
	if berr != nil {
		return browserPodState{}, berr
	}
	podKey := types.NamespacedName{Namespace: sess.Namespace, Name: desired.Name}
	if err := r.Client.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
		return browserPodState{}, fmt.Errorf("create browser pod %q: %w", desired.Name, err)
	}
	var live corev1.Pod
	if err := r.Client.Get(ctx, podKey, &live); err != nil {
		if errors.IsNotFound(err) {
			// Created but not yet observable in the cache; reflect next pass.
			return browserPodState{}, nil
		}
		return browserPodState{}, fmt.Errorf("get browser pod %q: %w", desired.Name, err)
	}
	if podReady(&live) && live.Status.PodIP != "" {
		return browserPodState{PodIP: live.Status.PodIP}, nil
	}
	// Not Ready. Distinguish "still starting" from "will never start" so the
	// caller can tell the agent and the user WHY the browser is missing instead
	// of waiting out a readiness deadline. sidecarPodFailure returns nil for a
	// pod that is merely still starting.
	return browserPodState{Failure: sidecarPodFailure(&live)}, nil
}

// cleanupOrphanedBrowserPods lists this session's pods and deletes any browser
// pod whose toolbox ref is NOT in currentRefs, together with that pod's
// NetworkPolicy and policy ConfigMap. The gate-token Secret is deliberately
// KEPT: a wake recreates the pod and reuses the same token (read-before-mint).
//
// Passing a nil/empty currentRefs (reapSessionPods) orphans every browser pod;
// passing the current resolved set (the main loop) reaps only a toolbox removed
// from the AgentClass mid-session. Mirrors cleanupOrphanedSidecarPods.
func (r *Reconciler) cleanupOrphanedBrowserPods(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, currentRefs map[string]bool) error {
	var podList corev1.PodList
	if err := r.Client.List(ctx, &podList,
		client.InNamespace(sess.Namespace),
		client.MatchingLabels{"agentprimitives.authzed.com/agentsession": sess.Name},
	); err != nil {
		return fmt.Errorf("list browser pods for session %q: %w", sess.Name, err)
	}
	logger := log.FromContext(ctx)
	var firstErr error
	for i := range podList.Items {
		pod := &podList.Items[i]
		ref, isBrowser := pod.Labels[labelBrowserToolbox]
		if !isBrowser {
			// Not a browser pod (runner, separate-pod sidecar, detector).
			continue
		}
		if currentRefs[ref] {
			continue // still in the resolved set — keep it.
		}
		if delErr := r.Client.Delete(ctx, pod); delErr != nil && !errors.IsNotFound(delErr) {
			logger.Info("cleanupOrphanedBrowserPods: delete pod failed",
				"session", sess.Namespace+"/"+sess.Name, "pod", pod.Name, "browsertoolbox", ref, "err", delErr.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("delete orphaned browser pod %q (ref %q): %w", pod.Name, ref, delErr)
			}
		} else {
			logger.Info("cleanupOrphanedBrowserPods: deleted orphaned browser pod",
				"session", sess.Namespace+"/"+sess.Name, "pod", pod.Name, "browsertoolbox", ref)
		}
		// Reap the pod's NetworkPolicy + policy ConfigMap alongside it (by their
		// deterministic names). NotFound is tolerated; the token Secret is NOT
		// deleted — a wake reuses it.
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: BrowserNetworkPolicyName(sess, ref)}}
		if delErr := r.Client.Delete(ctx, np); delErr != nil && !errors.IsNotFound(delErr) {
			logger.Info("cleanupOrphanedBrowserPods: NetworkPolicy delete failed",
				"session", sess.Namespace+"/"+sess.Name, "networkpolicy", np.Name, "browsertoolbox", ref, "err", delErr.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("delete orphaned browser NetworkPolicy %q (ref %q): %w", np.Name, ref, delErr)
			}
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: sess.Namespace, Name: BrowserPolicyConfigMapName(sess, ref)}}
		if delErr := r.Client.Delete(ctx, cm); delErr != nil && !errors.IsNotFound(delErr) {
			logger.Info("cleanupOrphanedBrowserPods: ConfigMap delete failed",
				"session", sess.Namespace+"/"+sess.Name, "configmap", cm.Name, "browsertoolbox", ref, "err", delErr.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("delete orphaned browser ConfigMap %q (ref %q): %w", cm.Name, ref, delErr)
			}
		}
	}
	return firstErr
}
