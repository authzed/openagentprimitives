// Package harness defines the agent-harness seam: the swappable component
// that runs an AgentSession's outer loop. ap-native wraps the built-in
// runner.Loop; other backends run a third-party agent binary. Backends
// self-register via an init() in their own package and are blank-imported by
// the binaries that need them (registry-over-branching).
//
// A harness supplies only its OWN contribution to the runner container —
// image, command, args, extra env and mounts. All platform wiring (memory
// token, NATS credentials, SpiceDB endpoint, signing keys) is computed by
// pkg/controllers/agentsession/podspec and merged around it, so every
// harness gets identical plumbing without restating it.
package harness

import (
	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// DefaultName is the harness used when AgentClass.spec.harness is absent.
// registry.Resolve maps an empty string to this default, but an
// empty-but-present spec.harness can't reach Resolve as "": the CRD field
// carries `+kubebuilder:validation:MinLength=1`, so the apiserver itself
// rejects a present-but-blank value before it ever reaches the controller.
const DefaultName = "ap-native"

// ModelAccessMode declares how a harness reaches its LLM.
type ModelAccessMode string

const (
	// Proxied: the harness honours a base-URL override and can be pointed at
	// AP's metering proxy, so token accounting, model pinning, and the
	// maxTokens budget all keep working.
	Proxied ModelAccessMode = "proxied"
	// Native: the harness calls its provider directly and AP cannot observe
	// those calls. The AgentSession carries a degraded-budget condition.
	Native ModelAccessMode = "native"
)

// HarnessOpts is the per-session input to Container. It carries the resolved
// image plus the endpoints AP exposes to the harness.
type HarnessOpts struct {
	// Session and Class are read-only; a harness must not mutate them.
	Session *spiceboxv1alpha1.AgentSession
	Class   *spiceboxv1alpha1.AgentClass

	// Image is the already-resolved container image for this harness.
	Image string

	// GovernedMCPEndpoint is the in-pod governed MCP server, a subprocess
	// harness's ONLY tool source. Empty for ap-native, which reaches the governor
	// in-process — and empty in practice, since podspec.go does not populate it
	// yet. A harness that needs it MUST make podspec.go set it rather than read
	// the empty value as a valid "no governor".
	GovernedMCPEndpoint string

	// ModelBaseURL is the in-pod metering proxy a Proxied harness points at (e.g.
	// via ANTHROPIC_BASE_URL). Empty when no proxy is running — and empty in
	// practice, since podspec.go does not populate it yet. A harness that needs
	// it MUST make podspec.go set it rather than read the empty value as a valid
	// "no proxy".
	ModelBaseURL string
}

// ContainerSpec is a harness's contribution to the runner container. The zero
// value is "no contribution beyond the image" — which is exactly ap-native,
// whose image ENTRYPOINT is the runner binary.
type ContainerSpec struct {
	// Command and Args override the image's ENTRYPOINT/CMD. Nil leaves the
	// image's own values in place.
	Command []string
	Args    []string

	// Env is merged into the platform env. A harness MUST NOT emit a name
	// that collides with platform wiring; podspec rejects collisions rather
	// than letting a harness shadow a credential path.
	Env []corev1.EnvVar

	// VolumeMounts and Volumes are appended to the runner container and pod.
	VolumeMounts []corev1.VolumeMount
	Volumes      []corev1.Volume
}

// Harness is the swappable outer loop. Implementations are stateless and safe
// for concurrent use — one instance serves every session.
type Harness interface {
	// Name is the registry key, matching AgentClass.spec.harness.
	Name() string

	// Container returns this harness's contribution to the runner container.
	// It must be a pure function of opts: no wall-clock, no randomness, no
	// I/O — the result feeds a server-side-applied Pod, so identical inputs
	// must yield byte-identical output.
	Container(opts HarnessOpts) (ContainerSpec, error)

	// ModelAccess declares whether this harness can be pointed at AP's metering
	// proxy. Cheap; no I/O. No caller reads it yet, so every harness's answer is
	// currently unobserved; a consumer that needs the mode reads this rather than
	// re-deriving it.
	ModelAccess() ModelAccessMode
}
