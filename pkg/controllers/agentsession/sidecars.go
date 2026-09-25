package agentsession

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	imagepin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
)

const (
	// RunModeInPod is the default run mode: the sidecar runs as a container
	// in the agent runner pod. A resolved sidecar with an empty RunMode is
	// treated as RunModeInPod.
	RunModeInPod = "in-pod"
	// RunModeSeparatePod is the run mode for a sidecar that must run in its
	// own pod, separate from the agent runner pod -- for either of two
	// independent reasons (see RunModeFor). BuildRunnerPod excludes these;
	// the controller handles them in later tasks.
	RunModeSeparatePod = "separate-pod"
)

// RunModeFor returns RunModeSeparatePod for either of two independent
// reasons, and RunModeInPod otherwise:
//
//   - The spec is secret-gated (any SecretInputs): its gating secret only
//     exists per-session, so it cannot be baked into the runner pod's spec.
//   - The spec declares Isolation=isolated: a toolbox with no gating secret
//     that still needs a separate pod's consequences -- its own
//     NetworkPolicy instead of the runner's, and reachability of
//     BuildSidecarPod's identity branch (see SidecarToolboxSpec.Isolation).
//
// Either reason alone is sufficient; a spec satisfying both still returns
// RunModeSeparatePod exactly once. Do not read one branch as the whole
// story -- a future third reason belongs here too, not layered on as a
// separate check at a call site.
func RunModeFor(spec spiceboxv1alpha1.SidecarToolboxSpec) string {
	if len(spec.SecretInputs) > 0 {
		return RunModeSeparatePod
	}
	if spec.Isolation == spiceboxv1alpha1.SidecarToolboxIsolationIsolated {
		return RunModeSeparatePod
	}
	return RunModeInPod
}

// ResolveSidecarImageRef decides the image ref a sidecar container launches.
//
// By-digest launch: once the SidecarToolbox controller records a baseline digest
// (pinDigest), rewrite the declared ref to "<repo>@<digest>" so the pod runs the
// exact bytes that were probed, regardless of any upstream retag.
//
// Local-dev exemption: on local/desktop clusters (usesLocalDevImages), first-
// party and sidecar images are `oap image load`ed into the node by their MUTABLE
// TAG and are NOT pullable by digest — there is no registry, and containerd
// cannot resolve "<repo>@<sha256:…>" against a tag-loaded image, so the sidecar
// container ErrImagePulls ("repository does not exist") and never starts. This
// mirrors `oap install --no-digest-pin`, which the desktop install already
// applies to first-party images; the per-session sidecar rewrite must honor the
// same rule or every SidecarToolbox with a locally-built image fails to boot.
//
// declared is the declared ref; pinDigest is the recorded baseline ("" when
// none). Returns declared unchanged when there is nothing to pin, when the ref
// is already digest-qualified, or when usesLocalDevImages is set.
func ResolveSidecarImageRef(declared, pinDigest string, usesLocalDevImages bool) string {
	if declared == "" || pinDigest == "" || usesLocalDevImages {
		return declared
	}
	if _, _, d := imagepin.SplitRef(declared); d != "" {
		return declared // already digest-qualified — leave the operator's assertion intact
	}
	return imagepin.WithDigest(declared, pinDigest)
}

// SidecarOpts is the input to BuildSidecarContainers.
type SidecarOpts struct {
	Resolved   []spiceboxv1alpha1.ResolvedSidecarToolbox
	SecretName func(ref string) string
}

// BuildSidecarContainers returns one container + any associated volumes
// per resolved SidecarToolbox. Container name is "sidecar-<ref>".
// MCP_PORT env, envFrom (secretRef), startupProbe, and securityContext
// are set per the design. ConfigMap volumes are returned alongside so the
// caller can append them to PodSpec.Volumes.
func BuildSidecarContainers(o SidecarOpts) ([]corev1.Container, []corev1.Volume, error) {
	if o.SecretName == nil {
		return nil, nil, fmt.Errorf("sidecars: SecretName fn is required")
	}
	containers := make([]corev1.Container, 0, len(o.Resolved))
	var volumes []corev1.Volume
	for i := range o.Resolved {
		rt := o.Resolved[i]
		c, vols, err := buildSidecarContainer(rt)
		if err != nil {
			return nil, nil, err
		}
		// In-pod sidecars receive their upstream credential via envFrom of the
		// per-session cred Secret materialized by the controller. (Separate-pod
		// sidecars do NOT use this path; they get secret-output keys wired
		// per-secretInput in BuildSidecarPod.)
		c.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: o.SecretName(rt.Ref)},
			},
		}}
		containers = append(containers, c)
		volumes = append(volumes, vols...)
	}
	return containers, volumes, nil
}

// buildSidecarContainer builds the container (and any source-projecting
// ConfigMap volumes) common to both the in-pod sidecar and the separate
// per-session sidecar pod. It delegates to cosidecar.BuildContainer — the
// canonical implementation shared with the content-guard detector. Credential
// wiring (in-pod envFrom vs separate-pod secret-output inputs) is layered on
// by the respective callers; EnvFrom is intentionally left empty here.
func buildSidecarContainer(rt spiceboxv1alpha1.ResolvedSidecarToolbox) (corev1.Container, []corev1.Volume, error) {
	s := cosidecar.Spec{
		Ref:  rt.Ref,
		Port: rt.Port,
		Health: cosidecar.Healthcheck{
			Path:           rt.Spec.Transport.Healthcheck.Path,
			TimeoutSeconds: rt.Spec.Transport.Healthcheck.TimeoutSeconds,
		},
		Config: rt.Spec.Config,
	}
	switch {
	case rt.Spec.Source.Image != "":
		s.Image = rt.Spec.Source.Image
	case rt.Spec.Source.Inline != nil:
		s.Inline = &cosidecar.InlineSource{
			BaseImage:     rt.Spec.Source.Inline.BaseImage,
			Entrypoint:    rt.Spec.Source.Inline.Entrypoint,
			ConfigMapName: rt.Spec.Source.Inline.Script.ConfigMapRef.Name,
			ConfigMapKey:  rt.Spec.Source.Inline.Script.ConfigMapRef.Key,
		}
	}
	return cosidecar.BuildContainer(s)
}

// mergeNetwork combines the SpiceboxClass network with the
// SidecarToolbox CR network into one effective policy. The merge rules:
//
//   - allowedHosts is the sorted, deduped union of class + CR.
//   - mode is the class's mode, upgraded to "allowlist" when the class
//     was "none" but the CR contributes any allowedHosts. (Never silently
//     downgrades from allowlist to none.)
//
// classOK signals whether the SpiceboxClass was successfully fetched —
// when false, the merge falls back to the CR's values alone.
//
// The returned values populate ResolvedSidecarToolbox.Effective* and are
// the source-of-truth a cluster network-policy controller should consume
// to enforce egress restrictions on the AgentSession pod. The operator
// itself does not enforce these in v1.
func mergeNetwork(classOK bool, classNet spiceboxv1alpha1.SpiceboxNetwork, crNet spiceboxv1alpha1.SidecarToolboxNetwork) (spiceboxv1alpha1.NetworkMode, []string) {
	hosts := map[string]struct{}{}
	if classOK {
		for _, h := range classNet.AllowedHosts {
			if h = strings.TrimSpace(h); h != "" {
				hosts[h] = struct{}{}
			}
		}
	}
	for _, h := range crNet.AllowedHosts {
		if h = strings.TrimSpace(h); h != "" {
			hosts[h] = struct{}{}
		}
	}
	out := make([]string, 0, len(hosts))
	for h := range hosts {
		out = append(out, h)
	}
	// Stable order for diffs + tests.
	sortStrings(out)

	var mode spiceboxv1alpha1.NetworkMode
	if classOK {
		mode = classNet.Mode
	}
	if mode == "" {
		mode = spiceboxv1alpha1.NetworkModeNone
	}
	if mode == spiceboxv1alpha1.NetworkModeNone && len(crNet.AllowedHosts) > 0 {
		mode = spiceboxv1alpha1.NetworkModeAllowlist
	}
	return mode, out
}

// MergeNetworkForTest exposes mergeNetwork for tests in the
// agentsession_test package without widening its package-level
// visibility.
func MergeNetworkForTest(classOK bool, classNet spiceboxv1alpha1.SpiceboxNetwork, crNet spiceboxv1alpha1.SidecarToolboxNetwork) (spiceboxv1alpha1.NetworkMode, []string) {
	return mergeNetwork(classOK, classNet, crNet)
}

// sortStrings is a tiny insertion-sort helper that keeps mergeNetwork
// import-free of "sort". The slices we sort are very small (one entry
// per allowed host).
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// EnvPrefix derives an env-var-legal name from an LLM prefix:
// uppercase + replace any non-[A-Za-z0-9_] character with underscore.
// Trailing underscores are trimmed (e.g. "abc-" → "ABC", not "ABC_").
func EnvPrefix(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-'a'+'A')
		case c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return strings.TrimRight(string(out), "_")
}

// AllocatePorts returns a deterministic port mapping for the given keys.
// v1: starts at 18080 and increments by index. Real conflict-detection
// against other containers in the pod isn't needed because each
// AgentSession pod has exactly one runner + N sidecars and the runner
// doesn't bind a TCP port itself.
func AllocatePorts(keys []string) map[string]int32 {
	out := make(map[string]int32, len(keys))
	port := int32(18080)
	for _, k := range keys {
		out[k] = port
		port++
	}
	return out
}
