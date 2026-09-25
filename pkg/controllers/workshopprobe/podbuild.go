// Package workshopprobe builds the hardened, one-shot pod (and its
// per-probe NetworkPolicy) a later controller runs to discover what tools a
// user-supplied image, script, or CLI actually exposes — WorkshopProbe's
// execution half. Everything here is a pure builder: no cluster calls, no
// side effects. The controller that applies these objects, watches the pod
// to completion, and writes WorkshopProbe.status is a separate task.
//
// The pod runs an image a workshop user chose, not one this cluster's
// operator vetted — the hardening in this file (no service-account token,
// read-only rootfs, dropped capabilities, a bounded lifetime, no image pull
// secret, and a NetworkPolicy that admits only the operator's own MCP
// tools/list call) is what makes running that image safe to attempt at all.
package workshopprobe

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/netpolrule"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

const (
	// ProbeMCPPort is the fixed port a probe's MCP target listens on — matches
	// the existing SidecarToolbox probe convention
	// (pkg/controllers/sidecartoolbox/probe.go's probePort) and the runtime
	// sidecar's own MCP_PORT, so an image built to run as a sidecar behaves
	// identically under a probe.
	ProbeMCPPort int32 = 8080

	// probePodFieldOwner is the SSA field manager the controller (a later
	// task) uses when applying the objects this package builds.
	probePodFieldOwner = "workshopprobe-controller"

	// defaultProbeTimeoutSeconds mirrors the CRD's own
	// +kubebuilder:default=120 on WorkshopProbeSpec.TimeoutSeconds. It only
	// matters for a WorkshopProbe built in-process without going through the
	// API server's defaulting (e.g. a unit test); every CR admitted by a real
	// cluster already carries a positive value.
	defaultProbeTimeoutSeconds = 120

	probeContainerName = "probe"
	// probeAppLabel identifies every probe pod this package builds,
	// regardless of which WorkshopProbe it belongs to — the label
	// BuildProbeNetworkPolicy's pod selector (together with the per-probe
	// name label) matches against.
	probeAppLabel = "ap-workshop-probe"
	// labelWorkshopProbe names the owning WorkshopProbe on the pod and the
	// NetworkPolicy — the second half of the pod selector, and how the
	// controller finds "the" pod/policy for one CR.
	labelWorkshopProbe = "agentprimitives.authzed.com/workshopprobe"

	// probeScriptVolumeName / probeScriptMountPath stage a Script-mode
	// probe's script content, read-only, from the ConfigMap named by
	// ProbeScriptConfigMapName.
	probeScriptVolumeName = "probe-script"
	probeScriptMountPath  = "/probe-script"
	// ProbeScriptConfigMapKey is the key under which the controller (a later
	// task) must write a Script-mode probe's script content — the pod this
	// package builds mounts exactly this key.
	ProbeScriptConfigMapKey = "run.sh"

	// tmpVolumeName / tmpMountPath give every probe container a writable
	// scratch directory despite ReadOnlyRootFilesystem: an arbitrary
	// user-supplied image or script cannot be assumed to run with no
	// writable path at all. Memory-backed and small — a probe is a
	// discovery call, not a workload.
	tmpVolumeName = "tmp"
	tmpMountPath  = "/tmp"
)

// ProbePodName returns the deterministic Pod name for a WorkshopProbe.
func ProbePodName(wp *spiceboxv1alpha1.WorkshopProbe) string {
	return wp.Name + "-probe"
}

// ProbeNetworkPolicyName returns the deterministic NetworkPolicy name for a
// WorkshopProbe's probe pod.
func ProbeNetworkPolicyName(wp *spiceboxv1alpha1.WorkshopProbe) string {
	return wp.Name + "-probe-netpol"
}

// ProbeScriptConfigMapName returns the deterministic ConfigMap name a
// Script-mode probe pod's script volume references. BuildProbePod only
// builds the Pod's reference to this ConfigMap by name — it cannot create
// the ConfigMap itself (a pure builder has no client) — so the controller
// (a later task) must create/update it, with the CR's spec.script.script
// content under key ProbeScriptConfigMapKey, before applying the Pod.
func ProbeScriptConfigMapName(wp *spiceboxv1alpha1.WorkshopProbe) string {
	return wp.Name + "-probe-script"
}

// probeOwnerRef owner-refs a probe-pod-family object to its WorkshopProbe so
// deleting the CR (or its namespace) reaps the pod/NetworkPolicy with it.
func probeOwnerRef(wp *spiceboxv1alpha1.WorkshopProbe) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         spiceboxv1alpha1.SchemeGroupVersion.String(),
		Kind:               "WorkshopProbe",
		Name:               wp.Name,
		UID:                wp.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// probeSelectorLabels returns the labels every probe pod for wp carries,
// and that BuildProbeNetworkPolicy's PodSelector matches against.
func probeSelectorLabels(wp *spiceboxv1alpha1.WorkshopProbe) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": probeAppLabel,
		labelWorkshopProbe:       wp.Name,
	}
}

// BuildProbePod renders the one-shot pod that runs wp's discriminated probe
// target: spec.image runs directly; spec.script runs spec.script.baseImage
// against the script staged into probeScriptMountPath from the ConfigMap
// named by ProbeScriptConfigMapName (the controller stages that ConfigMap
// separately — see its doc comment); spec.cliHelp runs
// spec.cliHelp.binary with spec.cliHelp.args and lets the controller read
// its logs. Exactly one of the three must be set — a CEL rule on
// WorkshopProbe.Spec (`workshopprobe_types.go`) enforces this at admission
// (no webhook), so a CR should never reach here with none or more than one
// set, but this function still fails closed rather than silently picking one.
//
// operatorNamespace is accepted for signature symmetry with
// BuildProbeNetworkPolicy — a later task's controller builds both objects
// for one WorkshopProbe from the same two positional arguments — and is not
// otherwise used here: the ingress peer that needs the operator's namespace
// belongs on the NetworkPolicy, not the pod.
func BuildProbePod(wp *spiceboxv1alpha1.WorkshopProbe, operatorNamespace string) (*corev1.Pod, error) {
	c := corev1.Container{
		Name:            probeContainerName,
		SecurityContext: podspec.HardenedContainerSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("500m"),
				corev1.ResourceMemory:           resource.MustParse("256Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tmpVolumeName, MountPath: tmpMountPath},
		},
	}
	volumes := []corev1.Volume{
		{
			Name: tmpVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: ptr.To(resource.MustParse("64Mi")),
				},
			},
		},
	}

	switch {
	case wp.Spec.Image != "":
		c.Image = wp.Spec.Image
		c.Env = []corev1.EnvVar{{Name: "MCP_PORT", Value: strconv.Itoa(int(ProbeMCPPort))}}
		c.Ports = []corev1.ContainerPort{{ContainerPort: ProbeMCPPort}}

	case wp.Spec.Script != nil:
		c.Image = wp.Spec.Script.BaseImage
		c.Env = []corev1.EnvVar{{Name: "MCP_PORT", Value: strconv.Itoa(int(ProbeMCPPort))}}
		c.Ports = []corev1.ContainerPort{{ContainerPort: ProbeMCPPort}}
		c.Command = []string{"/bin/sh", probeScriptMountPath + "/" + ProbeScriptConfigMapKey}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name:      probeScriptVolumeName,
			MountPath: probeScriptMountPath,
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: probeScriptVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: ProbeScriptConfigMapName(wp)},
					Items: []corev1.KeyToPath{
						{Key: ProbeScriptConfigMapKey, Path: ProbeScriptConfigMapKey},
					},
				},
			},
		})

	case wp.Spec.CliHelp != nil:
		c.Image = wp.Spec.CliHelp.Image
		c.Command = []string{wp.Spec.CliHelp.Binary}
		c.Args = wp.Spec.CliHelp.Args

	default:
		return nil, fmt.Errorf("workshopprobe %s/%s: spec has none of image, script, cliHelp set", wp.Namespace, wp.Name)
	}

	deadline := int64(wp.Spec.TimeoutSeconds)
	if deadline <= 0 {
		deadline = defaultProbeTimeoutSeconds
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ProbePodName(wp),
			Namespace:       wp.Namespace,
			Labels:          probeSelectorLabels(wp),
			OwnerReferences: []metav1.OwnerReference{probeOwnerRef(wp)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			ActiveDeadlineSeconds:        ptr.To(deadline),
			SecurityContext:              podspec.HardenedPodSecurityContext(),
			Containers:                   []corev1.Container{c},
			Volumes:                      volumes,
			// No ImagePullSecrets: a probe pod never carries the cluster's own
			// pull credentials into a user-chosen (possibly untrusted) image's
			// pull path. A private image simply fails to pull — surfaced as a
			// pod failure, not a leaked credential.
		},
	}, nil
}

// controlPlanePeer selects one control-plane workload in the operator
// namespace by its app.kubernetes.io/name label. Mirrors the unexported
// helper of the same name and shape in
// pkg/controllers/agentsession/netpol.go:117 — duplicated rather than
// imported because that helper is unexported and NetworkPolicy peer
// selection is cheap to keep in lockstep by hand.
func controlPlanePeer(operatorNamespace, appName string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": operatorNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/name": appName},
		},
	}
}

// BuildProbeNetworkPolicy isolates one WorkshopProbe's probe pod: ingress
// admits only the operator (spicebox-operator, in operatorNamespace) on
// ProbeMCPPort — the only caller with any reason to reach a probe pod is
// the controller collecting its tools/list result; egress is DNS plus
// external TCP 443 only — enough to pull the image and, for spec.image /
// spec.script, for the target itself to reach out over HTTPS if it needs
// to, but never port 80 and never the apiserver, neither of which a probe
// needs.
func BuildProbeNetworkPolicy(wp *spiceboxv1alpha1.WorkshopProbe, operatorNamespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            ProbeNetworkPolicyName(wp),
			Namespace:       wp.Namespace,
			Labels:          map[string]string{labelWorkshopProbe: wp.Name},
			OwnerReferences: []metav1.OwnerReference{probeOwnerRef(wp)},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: probeSelectorLabels(wp)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{controlPlanePeer(operatorNamespace, "spicebox-operator")},
				Ports: []networkingv1.NetworkPolicyPort{netpolrule.Port(corev1.ProtocolTCP, ProbeMCPPort)},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				netpolrule.DNSEgress(),
				{
					To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}},
					Ports: []networkingv1.NetworkPolicyPort{netpolrule.Port(corev1.ProtocolTCP, 443)},
				},
			},
		},
	}
}

// validateProbeImage refuses an empty or unparseable ref, and refuses a ref
// that names this cluster's own first-party trusted image registry —
// running an image FROM the same namespace the cluster's own components are
// pulled from is refused even though nothing here trusts probe images
// generally: it is the one image identity a probe's caller could use to
// make an untrusted pod look first-party in logs/audits, and there is never
// a legitimate reason for a workshop probe to target it.
func validateProbeImage(ref, trustedImageRegistry string) error {
	if ref == "" {
		return fmt.Errorf("probe image ref is empty")
	}
	// Parse only to confirm ref is a well-formed reference at all — the
	// registry-match decision below deliberately does NOT use this parsed
	// value; see refUnderTrustedRegistry's doc comment for why.
	if _, err := name.ParseReference(ref, name.WeakValidation); err != nil {
		return fmt.Errorf("probe image ref %q could not be parsed: %w", ref, err)
	}
	if trustedImageRegistry == "" {
		return nil
	}
	if refUnderTrustedRegistry(ref, trustedImageRegistry) {
		return fmt.Errorf(
			"probe image ref %q names this cluster's own trusted first-party registry %q; a probe must target an external image",
			ref, trustedImageRegistry)
	}
	return nil
}

// refUnderTrustedRegistry decides whether ref names an image under
// trustedImageRegistry, deriving the comparison from the RAW, unparsed ref —
// never from name.ParseReference's normalized Repository — the same "decide
// from the raw ref, never a normalized field" discipline
// pkg/controllers/webhooks/workshop/webhook.go's checkSidecarImage/refShape
// landed on after its own bypass (see that function's FIX ROUND 2 comment).
//
// The reason a normalized comparison is unsafe here specifically:
// name.ParseReference silently rewrites the registry alias "docker.io" to
// "index.docker.io" (go-containerregistry's own NewRegistry), but only on
// the REF side of a naive comparison — a trustedImageRegistry string
// configured with the other spelling of the same registry would then never
// string-match the normalized ref, letting an image that IS under the
// trusted registry slip through under whichever alias the operator didn't
// happen to configure. The fix canonicalizes the registry HOST token on
// BOTH sides through the identical alias mapping, each derived from that
// side's own raw text — so "docker.io/ap/x" and "index.docker.io/ap/x"
// collapse onto each other consistently regardless of which spelling
// trustedImageRegistry was configured with, while a genuinely different
// registry (or a ref naming no registry at all) never collapses onto it.
// Segment-boundary matching (splitRawRegistry's rest + a "/" boundary) is
// preserved so ".../apple" is never mistaken for ".../ap".
func refUnderTrustedRegistry(ref, trustedImageRegistry string) bool {
	refHost, refRest, refOK := splitRawRegistry(stripTagOrDigest(ref))
	if !refOK {
		// ref names no registry at all (a bare name, or an implicit
		// Docker-Hub-org form with no registry-shaped first segment) — it
		// can never BE this cluster's own explicitly-hosted registry.
		return false
	}
	trustHost, trustRest, trustOK := splitRawRegistry(trustedImageRegistry)
	if !trustOK {
		// trustedImageRegistry configured as a bare host with no path
		// (e.g. "ghcr.io", no org prefix): treat the whole string as the
		// registry host with an empty rest. This funnels into the same
		// trustRest == "" case below as a registry-shaped bare host (e.g.
		// "ghcr.io", which splitRawRegistry already parses as
		// (host="ghcr.io", rest="", ok=true)) — both mean "match every
		// repository under this host".
		trustHost, trustRest = trustedImageRegistry, ""
	}
	// Host comparison is case-insensitive: DNS/registry hosts are not
	// case-sensitive (RFC 1035), so a ref spelled with a different case than
	// trustedImageRegistry (e.g. "GHCR.IO") still names the same registry and
	// must not bypass this guard via case alone.
	if !strings.EqualFold(canonicalRegistryHost(refHost), canonicalRegistryHost(trustHost)) {
		return false
	}
	if trustRest == "" {
		// A bare-host trustedImageRegistry (no org/path) trusts every
		// repository under that host — the "oap install --image-registry
		// <registry>" bare-host shape (e.g. "ghcr.io") must refuse ANY path
		// under it, not just an exact empty-path match.
		return true
	}
	return refRest == trustRest || strings.HasPrefix(refRest, trustRest+"/")
}

// stripTagOrDigest removes a trailing ":tag" or "@digest" suffix from a raw
// image ref, leaving "[registry/]repository". A "/" never appears inside a
// tag or a digest (Docker's reference grammar forbids it), so the last ':'
// occurring AFTER the last '/' is always a tag delimiter, never part of a
// "host:port" registry authority that precedes the first '/'.
func stripTagOrDigest(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		return ref[:colon]
	}
	return ref
}

// splitRawRegistry splits a raw, unparsed "[registry/]repository" string
// (tag/digest already stripped) into its registry host and the remaining
// path, using the same raw heuristic as refShape in
// pkg/controllers/webhooks/workshop/webhook.go: the first "/"-delimited
// segment names a registry iff it is "localhost" or contains "." or ":" —
// the same test name.ParseReference itself uses internally to decide
// whether to split a registry off the front of a reference, reproduced here
// (that package exposes no public helper for it) so the decision is made on
// s's own literal text, never on a parsed/defaulted field. ok is false when
// s carries no registry-shaped segment at all (a bare name, or an implicit
// Docker-Hub-org form).
func splitRawRegistry(s string) (host, rest string, ok bool) {
	first, tail, found := strings.Cut(s, "/")
	if !found {
		if s != "" && (s == "localhost" || strings.ContainsAny(s, ".:")) {
			return s, "", true
		}
		return "", "", false
	}
	if first != "localhost" && !strings.ContainsAny(first, ".:") {
		return "", "", false
	}
	return first, tail, true
}

// canonicalRegistryHost normalizes just the well-known docker.io ↔
// index.docker.io registry alias (go-containerregistry's own
// DefaultRegistry/defaultRegistryAlias mapping in
// pkg/name.NewRegistry) — every other host passes through unchanged.
// Delegates to NewRegistry so this repo doesn't hand-roll the alias table,
// but calls it on a single, already-isolated raw host token — never on a
// full reference — so it inherits none of NewRegistry's
// defaulting-when-empty behavior (an empty host here would be a bug in the
// caller, not "no registry specified").
func canonicalRegistryHost(host string) string {
	if host == "" {
		return ""
	}
	reg, err := name.NewRegistry(host, name.WeakValidation)
	if err != nil {
		return host
	}
	return reg.RegistryStr()
}
