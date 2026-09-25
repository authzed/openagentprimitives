// Package cosidecar builds the Kubernetes objects for a container that runs as
// a co-located sidecar in (or beside) the runner pod: the hardened container,
// its startup probe, and a NetworkPolicy with runner-only ingress and an egress
// policy chosen by the caller. Both SidecarToolbox (tools, allowlist egress) and
// the content-guard detector (no egress) consume it — the builders contain no
// per-consumer branches.
package cosidecar

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/netpolrule"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// CredentialSecretName is the per-session Secret a SidecarToolbox's container
// envFroms: the upstream credentials the AgentSession reconciler resolved and
// materialized for toolbox `ref` in session `sessionName`.
//
// Spelled here, once, because two unrelated callers must agree on it and
// neither can discover it any other way. The reconciler builds the Secret under
// this name; `oap session capture` must read the same Secret's VALUE so the
// leak scan can look for it in the bytes it is about to write into a repo. A
// second literal in the capture would be a name that silently stops matching
// the moment this one changes, and the failure would be a credential committed
// to the repo with the scan reporting clean.
func CredentialSecretName(sessionName, ref string) string {
	return fmt.Sprintf("agentsession-%s-toolbox-%s", sessionName, ref)
}

// labelSidecar is the label key that identifies a co-sidecar pod by its ref.
// The value must match the string literal used by BuildRunnerNetworkPolicy and
// BuildSidecarNetworkPolicy in the parent package so that NetworkPolicy peer
// selectors agree.
const labelSidecar = "agentprimitives.authzed.com/sidecartoolbox"

// labelSession is the label key that carries the owning AgentSession name on
// every per-session pod and NetworkPolicy.
const labelSession = "agentprimitives.authzed.com/agentsession"

// InlineSource is an inline (ConfigMap-script) container source.
type InlineSource struct {
	BaseImage     string
	Entrypoint    []string
	ConfigMapName string
	ConfigMapKey  string
}

// Healthcheck configures the /healthz startup probe.
type Healthcheck struct {
	Path           string // default "/healthz"
	TimeoutSeconds int32  // default 30
}

// EgressPolicy selects the sidecar's NetworkPolicy egress. Denied ⇒ zero egress
// rules (deny-all). Otherwise the allowlist mode drives DNS + coarse 443/80.
type EgressPolicy struct {
	Denied        bool
	AllowlistMode spiceboxv1alpha1.NetworkMode

	// AllowedHosts is NOT ENFORCED and is never read by this package. A stock
	// Kubernetes NetworkPolicy cannot express hostnames, so BuildNetworkPolicy
	// consults only Denied and AllowlistMode — which means `allowlist` mode
	// permits TCP 443/80 to 0.0.0.0/0 regardless of what is listed here.
	//
	// It is carried so callers populating a Spec from
	// AgentSession.status.resolvedSidecarToolboxes[].effectiveAllowedHosts have
	// somewhere to put it, and so a future DNS-aware policy backend has the data
	// at hand. Anyone skimming the callers (netpol.go's BuildSidecarNetworkPolicy
	// populates this field) would otherwise reasonably read it as enforcement.
	AllowedHosts []string
}

// Spec is the neutral descriptor both consumers build.
type Spec struct {
	Ref string
	// Name is the NetworkPolicy object name. If empty, BuildNetworkPolicy
	// derives a default: "sidecar-" + sess.Name + "-" + Ref.
	Name      string
	Image     string
	Inline    *InlineSource
	Port      int32
	Health    Healthcheck
	Egress    EgressPolicy
	EnvFrom   string          // optional per-session Secret name (envFrom)
	RunnerEnv []corev1.EnvVar // injected into the runner container by the caller
	// Config is an opaque config delivered as AP_SIDECAR_CONFIG; empty means
	// the env var is not set.
	Config string
	// ImagePullSecret is an optional pull Secret name added to pods built by
	// BuildPod. When empty, ImagePullSecrets is left nil (unchanged behavior).
	ImagePullSecret string
}

// BuildContainer builds the hardened sidecar container (+ any inline-source
// ConfigMap volumes). Mirrors the prior buildSidecarContainer behavior.
func BuildContainer(s Spec) (corev1.Container, []corev1.Volume, error) {
	c := corev1.Container{
		Name:            "sidecar-" + s.Ref,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             []corev1.EnvVar{{Name: "MCP_PORT", Value: fmt.Sprintf("%d", s.Port)}},
		SecurityContext: defaultSecurityContext(),
		StartupProbe:    buildStartupProbe(s.Port, s.Health),
		// Make a crashed cosidecar's REAL cause readable from Pod status.
		//
		// The kubelet's CrashLoopBackOff waiting-message is only "back-off Ns
		// restarting failed container=…" — it names no cause, so a caller that
		// surfaces it (the sidecar/detector terminal-failure paths) can only
		// tell the user "it crashed". The default File policy does not help
		// either: it reads /dev/termination-log, which none of these images
		// write. FallbackToLogsOnError makes the kubelet copy the container's
		// log tail into lastState.terminated.message on any non-zero exit, so
		// the operator reads the actual startup error off the Pod it ALREADY
		// watches — no pods/log RBAC, no log streaming.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	if s.EnvFrom != "" {
		c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: s.EnvFrom}}}}
	}
	// AP_SIDECAR_CONFIG mirrors the MCP_PORT posture above: set only when the
	// caller has something to deliver. internal/cmd/apiadapter/main.go's
	// loadFromEnv reads this exact name and treats its absence as fatal, so a
	// sidecar that never sets Config never sees the var at all rather than
	// seeing it set to "".
	if s.Config != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "AP_SIDECAR_CONFIG", Value: s.Config})
	}
	var volumes []corev1.Volume
	switch {
	case s.Image != "":
		c.Image = s.Image
	case s.Inline != nil:
		c.Image = s.Inline.BaseImage
		c.Command = s.Inline.Entrypoint
		volName := "src-" + s.Ref
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: "/app",
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.Inline.ConfigMapName},
					Items: []corev1.KeyToPath{{
						Key:  s.Inline.ConfigMapKey,
						Path: s.Inline.ConfigMapKey,
					}},
				},
			},
		})
	default:
		return corev1.Container{}, nil, fmt.Errorf("cosidecar %q: source has neither image nor inline", s.Ref)
	}

	// Writable scratch at /tmp. defaultSecurityContext sets
	// ReadOnlyRootFilesystem, but many runtimes need a usable temp dir on
	// startup — the Python/torch prompt-injection detector crash-loops with
	// "No usable temporary directory found" before it can even import. An
	// emptyDir keeps the rest of the rootfs read-only while giving /tmp a
	// writable mount. The volume name is per-ref so multiple in-pod sidecars
	// sharing the runner pod do not collide on a single "tmp" volume name.
	tmpVol := "tmp-" + s.Ref
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: tmpVol, MountPath: "/tmp"})
	volumes = append(volumes, corev1.Volume{
		Name:         tmpVol,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	// Deny this container the pod's ServiceAccount token.
	//
	// A cosidecar runs an image taken verbatim from a user-supplied CR
	// (SidecarToolbox.spec.source.image) — a third-party MCP server, and a
	// supply-chain target. In the SEPARATE-POD and detector shapes the pod
	// itself sets AutomountServiceAccountToken=false, so there is no token to
	// get. But an in-pod cosidecar shares the RUNNER's pod, which deliberately
	// leaves automount on because the runner calls the Kubernetes API with it
	// — and the ServiceAccount admission plugin then injects that token into
	// every container in the pod, handing the third-party image the full
	// runner Role: patch on the AgentSession and its /status (the
	// effectiveIdentityMode and audit-key fields the identity webhook exists
	// to protect), get on the bound Channel's credentials Secret, get on the
	// class AgentIdentity's credential Secrets.
	//
	// Shadowing the mount path is the suppression the admission plugin
	// honours: it skips any container that already declares a volumeMount at
	// DefaultAPITokenMountPath. Applied unconditionally rather than only on
	// the in-pod path, because a cosidecar never needs the token in ANY shape
	// — making the safe case the only representable one, rather than
	// something a future caller has to remember. In the automount=false pods
	// it is an inert empty directory.
	saVol := "no-sa-token-" + s.Ref
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
		Name:      saVol,
		MountPath: saTokenMountPath,
		ReadOnly:  true,
	})
	volumes = append(volumes, corev1.Volume{
		Name:         saVol,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	return c, volumes, nil
}

// saTokenMountPath is where the kubelet projects a pod's ServiceAccount token
// (Kubernetes' DefaultAPITokenMountPath). A container that mounts something
// else here is skipped by the ServiceAccount admission plugin and never
// receives the token.
const saTokenMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

func defaultSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),
		ReadOnlyRootFilesystem:   ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func buildStartupProbe(port int32, h Healthcheck) *corev1.Probe {
	path := h.Path
	if path == "" {
		path = "/healthz"
	}
	timeout := h.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt(int(port)),
			},
		},
		PeriodSeconds:    1,
		FailureThreshold: timeout, // PeriodSeconds=1 → FailureThreshold equals seconds-to-give-up
	}
}

// BuildNetworkPolicy returns runner-only ingress on s.Port and egress per s.Egress.
// If s.Name is non-empty it is used as the NetworkPolicy object name; otherwise
// the default "sidecar-" + sess.Name + "-" + s.Ref is used.
func BuildNetworkPolicy(sess *spiceboxv1alpha1.AgentSession, s Spec) *networkingv1.NetworkPolicy {
	name := s.Name
	if name == "" {
		name = "sidecar-" + sess.Name + "-" + s.Ref
	}
	ingress := []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelSession: sess.Name},
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: labelSidecar, Operator: metav1.LabelSelectorOpDoesNotExist,
				}},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{{
			Protocol: ptr.To(corev1.ProtocolTCP),
			Port:     ptr.To(intstr.FromInt(int(s.Port))),
		}},
	}}
	var egress []networkingv1.NetworkPolicyEgressRule
	if !s.Egress.Denied {
		egress = AllowlistEgressRules(s.Egress.AllowlistMode)
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       sess.Namespace,
			OwnerReferences: ownerRef(sess),
			Labels:          map[string]string{labelSession: sess.Name, labelSidecar: s.Ref},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{labelSession: sess.Name, labelSidecar: s.Ref},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     ingress,
			Egress:      egress,
		},
	}
}

// BuildPod wraps BuildContainer in a per-session pod carrying the session owner
// ref and the {session, sidecar:Ref} labels the NetworkPolicy selects. Used by
// separate-pod consumers (the content-guard detector); in-pod consumers use
// BuildContainer + the runner pod directly.
func BuildPod(sess *spiceboxv1alpha1.AgentSession, s Spec) (*corev1.Pod, []corev1.Volume, error) {
	c, vols, err := BuildContainer(s)
	if err != nil {
		return nil, nil, err
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "sidecar-" + sess.Name + "-" + s.Ref,
			Namespace:       sess.Namespace,
			OwnerReferences: ownerRef(sess),
			Labels:          map[string]string{labelSession: sess.Name, labelSidecar: s.Ref},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: ptr.To(false),
			Containers:                   []corev1.Container{c},
			Volumes:                      vols,
			ImagePullSecrets:             PullSecretRefs(s.ImagePullSecret),
		},
	}
	return pod, vols, nil
}

// PullSecretRefs returns the imagePullSecrets list for an optional secret name
// (nil when empty, so the default behavior is unchanged).
func PullSecretRefs(name string) []corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}

// OwnerRef returns an owner reference slice pointing to sess as the controller.
// This is the canonical body moved from agentsession.sessionOwnerRef; the
// parent package's sessionOwnerRef delegates here.
func OwnerRef(sess *spiceboxv1alpha1.AgentSession) []metav1.OwnerReference {
	return []metav1.OwnerReference{{
		APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:               "AgentSession",
		Name:               sess.Name,
		UID:                sess.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}}
}

// ownerRef is the package-internal alias so callers within this package
// do not need to use the exported name.
func ownerRef(sess *spiceboxv1alpha1.AgentSession) []metav1.OwnerReference {
	return OwnerRef(sess)
}

// AllowlistEgressRules maps a merged network mode to its egress rules: none
// (or empty — fail-closed) yields nil; allowlist yields DNS + coarse TCP 443/80
// anywhere (hostnames are not expressible in vanilla NetworkPolicy;
// allowed-hosts lists on status remain the source of truth for DNS-aware CNIs
// to narrow further). This is the canonical body moved from
// agentsession.modeEgressRules; the parent package's modeEgressRules delegates
// here.
func AllowlistEgressRules(mode spiceboxv1alpha1.NetworkMode) []networkingv1.NetworkPolicyEgressRule {
	if mode != spiceboxv1alpha1.NetworkModeAllowlist {
		return nil
	}
	return []networkingv1.NetworkPolicyEgressRule{
		netpolrule.DNSEgress(),
		{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
			Ports: []networkingv1.NetworkPolicyPort{
				netpolrule.Port(corev1.ProtocolTCP, 443),
				netpolrule.Port(corev1.ProtocolTCP, 80),
			},
		},
	}
}
