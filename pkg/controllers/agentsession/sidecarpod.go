// pkg/controllers/agentsession/sidecarpod.go
//
// Separate per-session sidecar pod. A secret-gated SidecarToolbox
// (any SecretInput) runs NOT as a container in the agent runner pod but in its
// own per-session pod, with the resolved secret injected at startup. Once the
// pod is Ready its PodIP is reflected into
// AgentSession.status.resolvedSidecarToolboxes[i].SidecarPodIP so the runner
// can reach it over the pod network.
//
// The content-guard detector is the other separate cosidecar pod, so its
// materializer (ensureDetectorPod) lives here too.
package agentsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/platform/podstatus"
)

const (
	// labelSidecarToolbox identifies the separate sidecar pod's toolbox ref.
	labelSidecarToolbox = "agentprimitives.authzed.com/sidecartoolbox"
	// fileDeliverPrefix is the "file:" prefix of a secretInput.Deliver that
	// selects file delivery (mount) over env delivery.
	fileDeliverPrefix = "file:"
)

// ensureDetectorPod materializes one content-guard detector: its
// deny-all-egress NetworkPolicy and the pod itself. Both writes are
// idempotent, so a re-reconcile is a no-op.
//
// The seam exists so the ORDER is assertable in a unit test: the detector runs
// a user-supplied image and its whole security property is that it cannot
// reach the network (cosidecar.EgressPolicy{Denied:true}), so the policy must
// be stamped before the pod is schedulable — same rule, and same reason, as
// the runner / sandbox / sidecar pods.
func (r *Reconciler) ensureDetectorPod(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, spec cosidecar.Spec, pod *corev1.Pod) error {
	if err := r.ensureDetectorNetworkPolicy(ctx, sess, spec); err != nil {
		return err
	}
	// Create (idempotent: ignore AlreadyExists).
	if err := r.Client.Create(ctx, pod); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create detector pod %q: %w", spec.Ref, err)
	}
	return nil
}

// SidecarPodName returns the deterministic name for a session's separate
// sidecar pod for the given toolbox ref: "<session>-sidecar-<ref>".
func SidecarPodName(sess *spiceboxv1alpha1.AgentSession, ref string) string {
	return sess.Name + "-sidecar-" + ref
}

// SecretTokenHash returns the lowercase hex SHA-256 of value. A change in the
// resolved secret value changes this hash; the controller stamps it on
// status so a future task can trigger pod replacement on rotation.
func SecretTokenHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// BuildSidecarPod builds the separate per-session pod that runs a secret-gated
// SidecarToolbox. The single container reuses buildSidecarContainer (image
// source, MCP_PORT env, hardened securityContext, /healthz startup probe on
// rt.Port). Each secretInput is injected from the per-session secret-output
// Secret (soSecretName) per its Deliver mode:
//
//   - "env"        → an env var named secretInput.Name, valueFrom the Secret
//     key secretInput.From.
//   - "file:/path" → a volume projecting the Secret key secretInput.From, mounted
//     so the value lands at /path inside the container.
//
// The pod is owner-ref'd to the AgentSession (Controller + BlockOwnerDeletion)
// so it is garbage-collected when the session is deleted, and uses
// RestartPolicyOnFailure (a long-running MCP server, unlike the one-shot probe
// pod which uses Never).
//
// v1 supports image-source toolboxes; inline (ConfigMap) source is handled by
// the shared buildSidecarContainer helper, mirroring the in-pod path.
//
// identity is the sidecar identity seam (spec §13): non-nil only for the ONE
// sidecar a Ready, session-owned Workshop CR names (see workshopIdentityFor).
// When non-nil, the pod runs as identity.ServiceAccount (with the automount
// still off — the projection below is explicit), gets a projected
// workshop-sa-token volume mounted read-only at
// /var/run/secrets/workshop/serviceaccount, envFrom's identity.TokenSecret IN
// ADDITION to credSecretName, and gets the sidecar's platform env
// (OPERATOR_MEMORY_URL + WORKSHOP_NAMESPACE/WORKSHOP_SESSION_NAMESPACE/
// WORKSHOP_SESSION_NAME/WORKSHOP_ID) so internal/cmd/workshop can reach the
// operator and knows which workshop namespace it is scoped to. operatorURL is
// the same value podspec.go stamps onto the runner as OPERATOR_MEMORY_URL;
// workshopNamespace is W, the Workshop's provisioned namespace
// (Workshop.status.namespace, "ws-<uid12>") — distinct from
// identity.ServiceAccount/TokenSecret, which name objects in sess.Namespace,
// not W. trustedImageRegistry is the operator's own trusted registry
// (Reconciler.TrustedImageRegistry) and lands as WORKSHOP_TRUSTED_IMAGE_REGISTRY
// — a registry hostname, not a credential — so internal/cmd/workshop's
// `inventory` tool can report apimage.WorkshopSidecarImages refs the builder
// can use verbatim; omitted entirely when empty (a local/desktop install has
// no trusted registry). All three are read only when identity is non-nil. A
// nil identity must produce a pod byte-identical to a build with no workshop
// concept at all — this is the guard that makes the seam invisible to every
// non-workshop sidecar.
func BuildSidecarPod(sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedSidecarToolbox, soSecretName, credSecretName string, identity *spiceboxv1alpha1.WorkshopSidecarIdentity, operatorURL, workshopNamespace, trustedImageRegistry string) (*corev1.Pod, error) {
	c, volumes, err := buildSidecarContainer(rt)
	if err != nil {
		return nil, fmt.Errorf("build sidecar container for %q: %w", rt.Ref, err)
	}

	// envFrom the per-session upstream-credential Secret (materialized by
	// materializeSidecarSecret from spec.upstreamAuth), so the sidecar gets its
	// upstream cred env var — e.g. TS_AUTHKEY for the tailscale-authkey provider,
	// which the entrypoint needs to bring the tunnel up. Empty name only when the
	// caller has no cred secret (echo example); the Secret is always written
	// (possibly empty) before the pod is created, so envFrom never dangles.
	if credSecretName != "" {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: credSecretName},
			},
		})
	}

	// Inject each secretInput from the per-session secret-output Secret.
	//
	// Mode 0o444 (world-read), NOT 0o400: a Kubernetes secret volume file is
	// owned by root:root regardless of defaultMode, and this sidecar container
	// runs as a fixed NON-root UID (runAsNonRoot, image USER 1000) with NO
	// fsGroup on the pod — so 0o400 (owner-read for root) is UNREADABLE by the
	// container and every file-delivered secret read fails with "permission
	// denied" (e.g. the dedicated-mcp sidecar could not open its kubeconfig).
	// The pod is single-container and single-tenant (the Secret is scoped to
	// this session's sidecar), so world-read within the pod exposes the cred to
	// nothing the sidecar process itself doesn't already hold.
	secretFileMode := int32(0o444)
	for i := range rt.Spec.SecretInputs {
		si := rt.Spec.SecretInputs[i]
		if filePath, ok := fileDeliverPath(si.Deliver); ok {
			// file:<path> → project the Secret key as a file at <path>. Mount
			// the volume at the path's directory and project the key to the
			// path's basename so exactly the target file appears.
			volName := "secret-" + rt.Ref + "-" + sanitizeVolumeName(si.From)
			dir := path.Dir(filePath)
			base := path.Base(filePath)
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  soSecretName,
						DefaultMode: &secretFileMode,
						Items: []corev1.KeyToPath{{
							Key:  si.From,
							Path: base,
						}},
					},
				},
			})
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: dir,
				ReadOnly:  true,
			})
			continue
		}
		// Default delivery (deliver == "env"): an env var named si.Name sourced
		// from the Secret key si.From.
		c.Env = append(c.Env, corev1.EnvVar{
			Name: si.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: soSecretName},
					Key:                  si.From,
				},
			},
		})
	}

	// The workshop identity seam (spec §13): only when a Ready, session-owned
	// Workshop names THIS sidecar does identity come in, and it lands as an
	// explicit projected-token volume/mount + an extra EnvFrom — never as
	// AutomountServiceAccountToken=true. A nil identity leaves the pod exactly
	// as it was before this seam existed.
	var saName string
	if identity != nil {
		saName = identity.ServiceAccount
		const workshopTokenVolume = "workshop-sa-token"
		const workshopTokenMountPath = "/var/run/secrets/workshop/serviceaccount"
		volumes = append(volumes, corev1.Volume{
			Name: workshopTokenVolume,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Path:              "token",
								ExpirationSeconds: ptr.To(int64(3600)),
							},
						},
						{
							// The cluster CA, from the kube-root-ca.crt ConfigMap
							// every namespace auto-carries. The pod runs with
							// automountServiceAccountToken off, so the default
							// /var/run/secrets/kubernetes.io/serviceaccount CA is
							// absent; the workshop image builds its API client
							// from THIS mount alone (token + ca.crt), so the CA
							// has to ride the same projected volume.
							ConfigMap: &corev1.ConfigMapProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
								Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
							},
						},
					},
				},
			},
		})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      workshopTokenVolume,
			MountPath: workshopTokenMountPath,
			ReadOnly:  true,
		})
		if identity.TokenSecret != "" {
			c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: identity.TokenSecret},
				},
			})
		}
		// The sidecar's platform env (internal/cmd/workshop reads all five):
		// OPERATOR_MEMORY_URL so it can reach the operator with the bearer
		// EnvFrom'd above, and its workshop identity — WORKSHOP_NAMESPACE/
		// WORKSHOP_ID both name W (the provisioned workshop namespace this
		// sidecar's SA token is RBAC-bounded to), WORKSHOP_SESSION_NAMESPACE/
		// WORKSHOP_SESSION_NAME name the BUILDER AgentSession (B/X) that owns
		// the workshop. Plain Value env vars, not secretKeyRef — none of these
		// is a credential.
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "OPERATOR_MEMORY_URL", Value: operatorURL},
			corev1.EnvVar{Name: "WORKSHOP_NAMESPACE", Value: workshopNamespace},
			corev1.EnvVar{Name: "WORKSHOP_SESSION_NAMESPACE", Value: sess.Namespace},
			corev1.EnvVar{Name: "WORKSHOP_SESSION_NAME", Value: sess.Name},
			corev1.EnvVar{Name: "WORKSHOP_ID", Value: workshopNamespace},
		)
		// WORKSHOP_TRUSTED_IMAGE_REGISTRY: a registry HOSTNAME, not a secret — it
		// exists so `inventory` can report apimage.WorkshopSidecarImages refs the
		// builder can use verbatim (registry-qualified, instead of a bare local
		// tag no remote cluster can pull). Omitted entirely when the operator has
		// no trusted registry (local/desktop), rather than sent as "".
		if trustedImageRegistry != "" {
			c.Env = append(c.Env, corev1.EnvVar{Name: "WORKSHOP_TRUSTED_IMAGE_REGISTRY", Value: trustedImageRegistry})
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SidecarPodName(sess, rt.Ref),
			Namespace: sess.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": sess.Name,
				labelSidecarToolbox:                        rt.Ref,
			},
			OwnerReferences: sessionOwnerRef(sess),
		},
		Spec: corev1.PodSpec{
			// A long-running MCP server: restart on crash (unlike the one-shot
			// probe pod, which uses Never).
			RestartPolicy: corev1.RestartPolicyOnFailure,
			// The sidecar is a user-supplied MCP server; it has no
			// business holding an apiserver credential, and its allowlist
			// egress (TCP 443 anywhere) would otherwise give a compromised
			// sidecar an authenticated path to the apiserver. This holds even
			// under a workshop identity: the ServiceAccount above is set for
			// the projected-token volume's audience/subject only, and the
			// automount stays off regardless.
			AutomountServiceAccountToken: ptr.To(false),
			ServiceAccountName:           saName,
			Containers:                   []corev1.Container{c},
			Volumes:                      volumes,
		},
	}
	return pod, nil
}

// fileDeliverPath returns the target file path and true when deliver is a
// "file:<path>" spec; otherwise ("", false) (env delivery).
func fileDeliverPath(deliver string) (string, bool) {
	if strings.HasPrefix(deliver, fileDeliverPrefix) {
		return strings.TrimPrefix(deliver, fileDeliverPrefix), true
	}
	return "", false
}

// reconcileSidecarPod idempotently creates the separate per-session sidecar
// pod and reflects its readiness. It returns the pod's PodIP once the pod is
// Ready (the runner can then reach the sidecar at http://<podIP>:<rt.Port>),
// or "" when the pod was created but is not Ready yet — the caller requeues to
// reflect the IP on a later pass.
//
// Token rotation (Task 7 Part A): priorHash is the SecretTokenHash recorded on
// the EXISTING status entry for this ref (empty on first creation). When a pod
// already exists AND priorHash differs from rt.SecretTokenHash (the producer
// re-emitted a different token value), the pod is carrying the stale token and
// must be REPLACED: this deletes the old pod and returns replacing=true so the
// caller requeues. The next reconcile recreates the pod at the same
// deterministic name with the now-updated per-session Secret mounted/env'd, and
// SidecarPodIP re-reflects once the new pod is Ready.
//
// The delete is guarded: if it fails (and is not already-gone), the error is
// returned and the OLD pod is left running — the consumer is never left
// pod-less mid-replace. An error is also returned for genuine create/get
// failures (surfaced by the caller as a sidecar boot failure).
//
// workshopNamespace is W (the workshop's provisioned namespace); it is passed
// through to BuildSidecarPod unconditionally and used only when identity is
// non-nil. r.OperatorURL and r.TrustedImageRegistry are threaded the same way
// — reconcileSidecarPod is BuildSidecarPod's one caller, so this is the
// minimal path from the Reconciler's own operator-URL / trusted-registry
// config to the pod spec.
func (r *Reconciler) reconcileSidecarPod(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, rt spiceboxv1alpha1.ResolvedSidecarToolbox, soSecretName, credSecretName, priorHash string, identity *spiceboxv1alpha1.WorkshopSidecarIdentity, workshopNamespace string) (sidecarPodState, error) {
	desired, berr := BuildSidecarPod(sess, rt, soSecretName, credSecretName, identity, r.OperatorURL, workshopNamespace, r.TrustedImageRegistry)
	if berr != nil {
		return sidecarPodState{}, berr
	}
	podKey := types.NamespacedName{Namespace: sess.Namespace, Name: desired.Name}

	// Token-rotation replacement: an existing pod carrying a stale token (its
	// recorded hash differs from the freshly-resolved one) must be torn down
	// before recreate. The pod spec is immutable post-create and the secret is
	// frozen at pod-create time (no JIT refresh), so swapping the value requires
	// a delete+recreate, not an in-place update.
	if priorHash != "" && priorHash != rt.SecretTokenHash {
		var stale corev1.Pod
		getErr := r.Client.Get(ctx, podKey, &stale)
		switch {
		case getErr == nil:
			// Pod exists with the old token → delete it and signal a requeue.
			// Guard: on a delete failure keep the old pod + surface the error.
			if delErr := r.Client.Delete(ctx, &stale); delErr != nil && !errors.IsNotFound(delErr) {
				return sidecarPodState{}, fmt.Errorf("replace sidecar pod %q on token rotation: delete stale pod: %w", desired.Name, delErr)
			}
			return sidecarPodState{Replacing: true}, nil
		case errors.IsNotFound(getErr):
			// No pod to replace (e.g. it was GC'd) — fall through to create the
			// fresh pod below with the new value.
		default:
			return sidecarPodState{}, fmt.Errorf("get sidecar pod %q for rotation check: %w", desired.Name, getErr)
		}
	}

	if err := r.Client.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
		return sidecarPodState{}, fmt.Errorf("create sidecar pod %q: %w", desired.Name, err)
	}
	// Re-read (covers both the just-created and pre-existing pod) to observe
	// readiness + PodIP. The pod spec is immutable post-create, so we do not
	// reconcile spec drift here.
	var live corev1.Pod
	if err := r.Client.Get(ctx, podKey, &live); err != nil {
		if errors.IsNotFound(err) {
			// Created but not yet observable in the cache; reflect on the next pass.
			return sidecarPodState{}, nil
		}
		return sidecarPodState{}, fmt.Errorf("get sidecar pod %q: %w", desired.Name, err)
	}
	if podReady(&live) && live.Status.PodIP != "" {
		return sidecarPodState{PodIP: live.Status.PodIP}, nil
	}
	// Not Ready. Distinguish "still starting" from "will never start": a pod
	// wedged in a terminal container state (crash loop, unpullable image,
	// unsatisfiable config) must not be waited on silently — it is reported so
	// the caller can tell the agent and the user WHY the toolset is missing.
	return sidecarPodState{Failure: sidecarPodFailure(&live)}, nil
}

// sidecarPodState is what one reconcile pass observed about a separate-pod
// sidecar's pod. Exactly one of PodIP / Replacing / Failure is meaningful per
// pass; all three zero means "created, still starting, check again next pass".
type sidecarPodState struct {
	// PodIP is set once the pod is Ready and has an IP — the runner can then
	// reach the sidecar.
	PodIP string
	// Replacing is true when a stale-token pod was just deleted and the
	// replacement will be created on the next pass.
	Replacing bool
	// Failure is non-nil when the pod is wedged in a state it will not recover
	// from on its own. See sidecarPodFailure.
	Failure *spiceboxv1alpha1.SidecarPodFailure
}

// podReady reports whether the pod has a PodReady=True condition.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// terminalPodWaitReason returns the first container Waiting state whose reason
// is terminal, so a caller can fail closed in seconds instead of waiting out a
// multi-minute readiness deadline. It is a thin wrapper over the shared
// podstatus classifier (the same rules the spiceboxsession bundle-pod path
// uses), kept here so this package's call sites and tests are unchanged.
func terminalPodWaitReason(pod *corev1.Pod) (reason, message string, ok bool) {
	return podstatus.TerminalContainerWaitReason(pod)
}

// sidecarPodFailure classifies a separate-pod sidecar's Pod into the
// operator-owned status record the runner surfaces to the agent and user, or
// nil when the pod is healthy or merely still starting.
//
// The message selection is the point of this function. For the dominant case —
// a container that starts, rejects its config, and exits — the kubelet's
// WAITING message is "back-off 2m40s restarting failed container=…", which
// names no cause and is useless to a user. The cause is in the container's log,
// which the kubelet copies into the LAST TERMINATED message because the pod is
// built with FallbackToLogsOnError (see cosidecar.BuildContainer). So the
// terminated message wins when present, and the waiting message is the fallback
// for containers that never ran at all (ImagePullBackOff) or produced no output.
func sidecarPodFailure(pod *corev1.Pod) *spiceboxv1alpha1.SidecarPodFailure {
	reason, waitMsg, ok := terminalPodWaitReason(pod)
	if !ok {
		return nil
	}
	f := &spiceboxv1alpha1.SidecarPodFailure{Reason: reason, Message: waitMsg}
	for _, cs := range pod.Status.ContainerStatuses {
		term := cs.LastTerminationState.Terminated
		if term == nil {
			continue
		}
		f.ExitCode = ptr.To(term.ExitCode)
		if msg := strings.TrimSpace(term.Message); msg != "" {
			f.Message = msg
		}
		break
	}
	return f
}

// secretInputBytes concatenates the per-session secret-output Secret's values
// for the toolbox's secretInputs in declaration order, joined by NUL so the
// hash is sensitive to key boundaries. Missing keys contribute empty bytes —
// the caller only reaches this once every key is satisfied, so in practice all
// are present. The result feeds SecretTokenHash for rotation detection.
func secretInputBytes(soSecret *corev1.Secret, inputs []spiceboxv1alpha1.SidecarToolboxSecretInput) []byte {
	var buf []byte
	for i, si := range inputs {
		if i > 0 {
			buf = append(buf, 0)
		}
		buf = append(buf, soSecret.Data[si.From]...)
	}
	return buf
}

// cleanupOrphanedSidecarPods lists all separate-pod sidecar pods owned by
// this session (label agentsession=<name>) and deletes any whose
// sidecartoolbox ref is NOT in currentSeparateRefs. This handles the
// toolbox-removed-mid-session case: when a SidecarToolbox is removed from
// the AgentClass, its separate pod (owned by the session, not the toolbox)
// would otherwise linger until session deletion. Each delete error is
// surfaced/logged rather than silently dropped.
func (r *Reconciler) cleanupOrphanedSidecarPods(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, currentSeparateRefs map[string]bool) error {
	var podList corev1.PodList
	if err := r.Client.List(ctx, &podList,
		client.InNamespace(sess.Namespace),
		client.MatchingLabels{"agentprimitives.authzed.com/agentsession": sess.Name},
	); err != nil {
		return fmt.Errorf("list sidecar pods for session %q: %w", sess.Name, err)
	}
	var firstErr error
	for i := range podList.Items {
		pod := &podList.Items[i]
		ref, hasSidecarLabel := pod.Labels[labelSidecarToolbox]
		if !hasSidecarLabel {
			// Not a sidecar pod (e.g. the runner pod, which shares the
			// agentsession label but has no sidecartoolbox label).
			continue
		}
		if currentSeparateRefs[ref] {
			// Still in the current resolved set — keep it.
			continue
		}
		// This sidecar's ref is no longer in the resolved set. Delete it.
		if delErr := r.Client.Delete(ctx, pod); delErr != nil && !errors.IsNotFound(delErr) {
			log.FromContext(ctx).Info("cleanupOrphanedSidecarPods: delete failed",
				"session", sess.Namespace+"/"+sess.Name,
				"pod", pod.Name,
				"sidecartoolbox", ref,
				"err", delErr.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("delete orphaned sidecar pod %q (ref %q): %w", pod.Name, ref, delErr)
			}
		} else {
			log.FromContext(ctx).Info("cleanupOrphanedSidecarPods: deleted orphaned sidecar pod",
				"session", sess.Namespace+"/"+sess.Name,
				"pod", pod.Name,
				"sidecartoolbox", ref)
		}
		// Reap the sidecar's NetworkPolicy alongside the pod. This delete runs
		// unconditionally — the policy may exist even when the pod was already
		// gone (or its delete just failed above). NotFound is tolerated
		// (stamping may be disabled, or the policy already gone); any other
		// delete error is logged + surfaced like the pod delete.
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Namespace: sess.Namespace,
			Name:      SidecarNetworkPolicyName(sess, ref),
		}}
		if delErr := r.Client.Delete(ctx, np); delErr != nil && !errors.IsNotFound(delErr) {
			log.FromContext(ctx).Info("cleanupOrphanedSidecarPods: NetworkPolicy delete failed",
				"session", sess.Namespace+"/"+sess.Name,
				"networkpolicy", np.Name,
				"sidecartoolbox", ref,
				"err", delErr.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("delete orphaned sidecar NetworkPolicy %q (ref %q): %w", np.Name, ref, delErr)
			}
		}
	}
	return firstErr
}

// sanitizeVolumeName lowercases and replaces any non-[a-z0-9-] character with
// '-' so a secret-output key is usable as part of a Kubernetes volume name
// (RFC 1123 label component).
func sanitizeVolumeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c-'A'+'a')
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return strings.Trim(string(out), "-")
}
