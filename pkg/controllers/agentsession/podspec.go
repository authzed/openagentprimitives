// Package agentsession contains pure-function helpers used by the AgentSession
// reconciler to build the per-session runner Pod, RBAC resources,
// and memory-token Secret.
package agentsession

import (
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/agent/harness"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession/cosidecar"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"
)

// runnerContainerName is the name of the agent-runner container inside the
// runner Pod. The pod is NOT single-container: in-pod SidecarToolboxes are
// appended as siblings, running images taken verbatim from a user-supplied CR.
// Every read of pod status that means "the runner" — readiness, restart count,
// crash detail — must select on this name rather than scanning the pod, or a
// third party's container speaks for the agent.
const runnerContainerName = "runner"

// PodSpecOpts is the input to BuildRunnerPod.
type PodSpecOpts struct {
	Session     *spiceboxv1alpha1.AgentSession
	Class       *spiceboxv1alpha1.AgentClass
	Image       string
	ServiceAcct string
	MemoryToken string // Secret name (key "token")
	OperatorURL string // e.g. "http://spicebox-operator.agentprimitives-system.svc:8082"
	NATSURL     string // e.g. "nats://spicebox-nats.agentprimitives-system.svc:4222"; only used when channel-attached

	// WebdBaseURL is webd's externally reachable base URL, carried across the
	// namespace boundary for the same reason SpiceDBEndpoint is: the ConfigMap
	// holding it lives in agentprimitives-system and runner pods do not.
	//
	// The runner uses it to compose the durable artifact link a trigger's
	// status surface carries. Empty is a LIVE state, not a misconfiguration —
	// `oap install` seeds the ConfigMap empty while it arranges external
	// access — and leaves the env var off the pod entirely, so the runner's
	// own "not addressable yet" default handles it in one place.
	WebdBaseURL string

	// ResolvedSidecars is the per-session toolbox snapshot; one entry per
	// AgentClass.spec.sidecarToolboxes ref. May be empty.
	ResolvedSidecars []spiceboxv1alpha1.ResolvedSidecarToolbox
	// SidecarSecretName returns the per-session Secret name for a toolbox
	// ref. The reconciler creates these before pod build; this lets
	// BuildRunnerPod wire envFrom on the sidecar containers.
	SidecarSecretName func(ref string) string

	// NATSCredsMounted is true when the per-session Secret carries the
	// nats.creds/nats.ca keys; gates the runner pod's NATS creds mounts
	// + path env vars. It must reflect the Secret's actual contents
	// (ground truth), not whether the operator holds a NATS identity —
	// a Secret minted in an earlier pass while the identity was absent
	// stays keyless even after the operator restarts with one. A SubPath
	// mount of an absent Secret key hangs the pod in ContainerCreating,
	// so the pod must declare these mounts iff the keys exist.
	NATSCredsMounted bool

	// SpiceDBEndpoint / SpiceDBInsecure are the operator's resolved
	// SpiceDB connection params (from pkg/authz/spicedb.LoadEnvConfig at
	// operator startup). They are value-copied onto each spawned runner
	// pod's env, because pods can only reference ConfigMaps/Secrets in
	// their own namespace and the install-time SpiceDB ConfigMap +
	// Secret live in agentprimitives-system. The operator process is
	// the single source of truth; this propagation just gets the
	// resolved values across the namespace boundary.
	//
	// The preshared token is intentionally NOT here: it rides in the
	// per-session Secret (key "spicedb-token", written by the
	// controller) and is mounted as a file the runner reads via
	// SPICEDB_TOKEN_PATH — never delivered as a plaintext env var.
	SpiceDBEndpoint string
	SpiceDBInsecure bool

	// ResolvedContentGuardDetectors carries the per-session detector
	// snapshot. Each entry with a non-empty PodIP produces a
	// CONTENTGUARD_DETECTOR_ENDPOINT env var on the runner container.
	// The reconciler gates runner-pod creation until every PodIP is
	// reflected, so by the time BuildRunnerPod is called all entries
	// are expected to have a non-empty PodIP; the guard here is purely
	// defensive.
	ResolvedContentGuardDetectors []spiceboxv1alpha1.ResolvedContentGuardDetector

	// ImagePullSecret is an optional pull Secret name added to the runner pod.
	// When empty, ImagePullSecrets is left nil (unchanged behavior).
	ImagePullSecret string

	// Harness is the resolved harness for this session. Required — the
	// factory resolves it fail-closed before calling BuildRunnerPod.
	Harness harness.Harness
	// HarnessImage is the image for Harness. Falls back to Image when empty,
	// which is the oap-native case (one runner image, operator-wide).
	HarnessImage string
}

// BuildRunnerPod returns the operator-built Pod for an AgentSession's runner.
// It returns an error (rather than panicking) when sidecar container
// construction fails: controller-runtime silently recovers panics, so a
// panic here would be swallowed and surface only as a stuck reconcile.
// Callers must propagate the error.
func BuildRunnerPod(o PodSpecOpts) (*corev1.Pod, error) {
	// 0o444 (world-read inside the pod) so the nonroot distroless runner
	// can read the projected Secret files. Tier 0 isolation is unchanged:
	// the volume is pod-scoped, never reaches the host or other pods.
	memMode := int32(0o444)
	keyMode := int32(0o444)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RunnerPodName(o.Session),
			Namespace: o.Session.Namespace,
			Labels: map[string]string{
				"agentprimitives.authzed.com/agentsession": o.Session.Name,
			},
			OwnerReferences: sessionOwnerRef(o.Session),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: o.ServiceAcct,
			RestartPolicy:      corev1.RestartPolicyOnFailure,
			// EnableServiceLinks=false: prevents the kubelet from injecting
			// cluster service env vars (e.g. FOO_SERVICE_HOST/PORT) that
			// bloat the environment and can shadow intentional runner vars.
			// Matches the sandbox pod (pkg/platform/podspec/builder.go).
			EnableServiceLinks: ptr.To(false),
			// SecurityContext: matches the sandbox pod. AutomountServiceAccountToken
			// is intentionally left at the default (nil = true) because the runner
			// calls the Kubernetes API using its ServiceAccount token.
			SecurityContext:  podspec.HardenedPodSecurityContext(),
			ImagePullSecrets: cosidecar.PullSecretRefs(o.ImagePullSecret),
			Containers: []corev1.Container{{
				Name: runnerContainerName,
				// Image is set once, below, after the harness block resolves
				// o.HarnessImage vs o.Image — not here, to avoid two
				// assignments to the same field.
				ImagePullPolicy: corev1.PullIfNotPresent,
				// On a non-zero exit, have the kubelet capture the tail of the
				// runner's logs into ContainerStatus.LastTerminationState's
				// Terminated.Message. That message is readable via the cached
				// client (unlike pod logs), so the AgentSession reconciler can
				// surface the runner's ACTUAL startup error to the user's
				// channel (e.g. an unresolved toolkit) instead of a generic
				// "runner restarted N times" — see reflectRunnerPod +
				// runnerCrashDetail.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				Args: []string{
					"--hostname=$(POD_NAME)",
				},
				// SecurityContext: matches the sandbox pod (pkg/platform/podspec/builder.go).
				// ReadOnlyRootFilesystem=true requires a writable emptyDir at /tmp
				// for Go's temp-file operations (see the "tmp" volume below).
				SecurityContext: podspec.HardenedContainerSecurityContext(),
				Env:             buildRunnerEnv(o),
				VolumeMounts:    buildRunnerVolumeMounts(o),
			}},
			Volumes: []corev1.Volume{
				{
					Name: "memory-token",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName:  o.MemoryToken,
							DefaultMode: &memMode,
						},
					},
				},
				{
					Name: "llm-api-key",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: o.Session.Status.EffectiveSettings.Model.APIKey.Name,
							Items: []corev1.KeyToPath{{
								Key:  o.Session.Status.EffectiveSettings.Model.APIKey.Key,
								Path: "api-key",
							}},
							DefaultMode: &keyMode,
						},
					},
				},
				// /tmp: required because ReadOnlyRootFilesystem=true. The runner
				// (a static Go binary on distroless) uses os.TempDir() → /tmp for
				// any ephemeral writes; without this the pod would crash on the
				// first temp-file operation. Matches the sandbox pod's /tmp volume.
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}
	if len(o.ResolvedSidecars) > 0 {
		// Separate-pod sidecars (secret-gated) are handled by the controller in
		// later tasks and must NOT be injected as containers into the agent pod.
		// Build a filtered slice without mutating o.ResolvedSidecars.
		inPod := make([]spiceboxv1alpha1.ResolvedSidecarToolbox, 0, len(o.ResolvedSidecars))
		for _, rt := range o.ResolvedSidecars {
			if rt.RunMode != RunModeSeparatePod {
				inPod = append(inPod, rt)
			}
		}
		if len(inPod) > 0 {
			sidecars, sidecarVolumes, err := BuildSidecarContainers(SidecarOpts{
				Resolved:   inPod,
				SecretName: o.SidecarSecretName,
			})
			if err != nil {
				return nil, fmt.Errorf("build sidecar containers: %w", err)
			}
			pod.Spec.Containers = append(pod.Spec.Containers, sidecars...)
			pod.Spec.Volumes = append(pod.Spec.Volumes, sidecarVolumes...)

			// Index 0 is the runner: it is the sole entry when pod.Spec.Containers
			// is constructed above, and sidecars are always appended after it,
			// never prepended. The harness block below uses a name lookup instead
			// (defensive against any future change to that ordering).
			runner := &pod.Spec.Containers[0]
			for _, rt := range inPod {
				runner.Env = append(runner.Env, corev1.EnvVar{
					Name:  "MCP_PORT_" + EnvPrefix(rt.Name),
					Value: strconv.Itoa(int(rt.Port)),
				})
			}
		}
	}
	// Content-guard detectors run as SEPARATE pods (zero egress), never as
	// in-pod containers. Once the operator reflects a detector pod's PodIP
	// into status, it sets CONTENTGUARD_DETECTOR_ENDPOINT on the runner so
	// the inspector can reach it. The reconciler holds runner-pod creation
	// until every detector PodIP is reflected; the PodIP guard below is
	// defensive for the edge case where this function is called before all
	// IPs are stamped (e.g., the in-process e2e factory).
	//
	// At most one inspector may implement DetectorProvider: a second
	// CONTENTGUARD_DETECTOR_ENDPOINT write would shadow the first. This is
	// rejected at admission by ContentInspectorsError (pkg/controllers/webhooks/settings),
	// so a configuration that reaches here has at most one detector.
	for _, det := range o.ResolvedContentGuardDetectors {
		if det.PodIP == "" {
			continue // reconciler holds runner creation until reflected; defensive
		}
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
			Name:  "CONTENTGUARD_DETECTOR_ENDPOINT",
			Value: fmt.Sprintf("http://%s:%d", det.PodIP, det.Port),
		})
	}
	// Ask the resolved harness for its own contribution and fold it in. A nil
	// Harness is a programmer error — the factory resolves fail-closed — so
	// surface it rather than silently building an ap-native pod.
	if o.Harness == nil {
		return nil, fmt.Errorf("BuildRunnerPod: no harness resolved for session %s/%s", o.Session.Namespace, o.Session.Name)
	}
	image := o.HarnessImage
	if image == "" {
		image = o.Image
	}
	hspec, herr := o.Harness.Container(harness.HarnessOpts{
		Session: o.Session,
		Class:   o.Class,
		Image:   image,
	})
	if herr != nil {
		return nil, fmt.Errorf("harness %q container: %w", o.Harness.Name(), herr)
	}
	runnerIdx := -1
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == runnerContainerName {
			runnerIdx = i
			break
		}
	}
	if runnerIdx < 0 {
		return nil, fmt.Errorf("BuildRunnerPod: runner container not found in built pod")
	}
	pod.Spec.Containers[runnerIdx].Image = image
	if err := mergeHarnessContainer(&pod.Spec.Containers[runnerIdx], hspec); err != nil {
		return nil, fmt.Errorf("harness %q: %w", o.Harness.Name(), err)
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, hspec.Volumes...)
	return pod, nil
}

// mergeHarnessContainer folds a harness's contribution into the
// platform-built runner container. Platform wiring is authoritative: a
// harness that emits an env name the platform already set is rejected rather
// than allowed to shadow a credential path or endpoint. Volumes are appended
// by the caller, which owns the Pod.
func mergeHarnessContainer(c *corev1.Container, spec harness.ContainerSpec) error {
	platform := make(map[string]struct{}, len(c.Env))
	for _, e := range c.Env {
		platform[e.Name] = struct{}{}
	}
	for _, e := range spec.Env {
		if _, dup := platform[e.Name]; dup {
			return fmt.Errorf("harness env %q collides with platform wiring", e.Name)
		}
	}
	if spec.Command != nil {
		c.Command = spec.Command
	}
	if spec.Args != nil {
		c.Args = spec.Args
	}
	c.Env = append(c.Env, spec.Env...)
	c.VolumeMounts = append(c.VolumeMounts, spec.VolumeMounts...)
	return nil
}

// RunnerPodName returns the deterministic pod name for s.
func RunnerPodName(s *spiceboxv1alpha1.AgentSession) string {
	return s.Name + "-runner"
}

// buildRunnerVolumeMounts returns the runner container's volumeMounts.
// The memory-token + llm-api-key mounts are always present. When
// o.NATSCredsMounted is true two additional read-only SubPath mounts
// project the per-session Secret's "nats.creds" and "nats.ca" keys.
// That flag is set by the controller from the per-session Secret's
// actual contents — NOT from InputChannel != nil: a channel-attached
// session whose Secret was minted while the operator lacked a NATS
// identity has no such keys. A SubPath mount whose key is absent
// fails the pod (CreateContainerConfigError / hangs in
// ContainerCreating), so the mounts must track the Secret's keys.
func buildRunnerVolumeMounts(o PodSpecOpts) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "memory-token", MountPath: "/var/run/agent/memory-token", SubPath: "token", ReadOnly: true},
		{Name: "llm-api-key", MountPath: "/var/run/agent/llm-api-key", SubPath: "api-key", ReadOnly: true},
		// SpiceDB preshared token. Lives in the same per-session Secret as
		// "token" (o.MemoryToken) under the "spicedb-token" key, so we
		// reuse the memory-token volume with a distinct SubPath. Unlike
		// the gated nats.creds mount, this is safe to be unconditional:
		// spicedb.LoadEnvConfig fails the operator at startup if the token
		// is empty, so the controller always has a non-empty token to write
		// into the per-session Secret — the "spicedb-token" key is
		// guaranteed present on every session Secret. The runner reads it
		// via SPICEDB_TOKEN_PATH.
		{Name: "memory-token", MountPath: "/var/run/agent/spicedb-token", SubPath: "spicedb-token", ReadOnly: true},
		// Per-session HMAC key for approval-grant arguments_hash values.
		// Same per-session Secret, distinct SubPath — and like
		// spicedb-token the key is guaranteed present (minted on the
		// create path, ensured on the update path for pre-upgrade
		// Secrets), so an unconditional mount cannot hang the pod.
		{Name: "memory-token", MountPath: "/var/run/agent/args-hash-key", SubPath: agentSessionSecretArgsHashKey, ReadOnly: true},
		// Per-session Ed25519 seed signing this session's append-only
		// audit entries. Same per-session Secret, distinct SubPath;
		// minted on create + ensured on update so the unconditional
		// mount cannot hang the pod.
		{Name: "memory-token", MountPath: "/var/run/agent/audit-signing-key", SubPath: agentSessionSecretAuditSigningKey, ReadOnly: true},
		// /tmp: writable scratch space required by ReadOnlyRootFilesystem=true.
		{Name: "tmp", MountPath: "/tmp"},
	}
	if o.NATSCredsMounted {
		// Both keys live in the same per-session Secret as "token"
		// (o.MemoryToken), so we reuse the memory-token volume with
		// distinct SubPaths rather than adding a second volume.
		mounts = append(mounts,
			corev1.VolumeMount{Name: "memory-token", MountPath: "/var/run/agent/nats-creds", SubPath: "nats.creds", ReadOnly: true},
			corev1.VolumeMount{Name: "memory-token", MountPath: "/var/run/agent/nats-ca", SubPath: "nats.ca", ReadOnly: true},
		)
	}
	return mounts
}

// buildRunnerEnv constructs the env-var list for the runner container.
// For channel-attached sessions (sess.Spec.InputChannel != nil) it appends
// defaultRunnerIdleTTL is how long a channel-attached runner waits for a new
// message before exiting, when the AgentClass declares no IdleTTL of its own.
//
// This value, not internal/cmd/runner's `--idle-ttl` flag default, is what production
// actually uses: buildRunnerEnv sets IDLE_TTL on EVERY channel-attached pod,
// and an explicit env var always wins over a flag default. Raising the flag
// alone therefore changes nothing — which is exactly what happened when the
// base TTL was raised to 15m and every session went on reaping at five
// minutes, because the operator was still stamping the old number.
//
// It matters more than a resource-tidiness knob now: an agent-defined UI's
// data bindings are answered BY THE RUNNER, so a reaped runner is a dashboard
// whose every section fails. The presence heartbeat extends this while someone
// is actually watching; this is the floor for a page nobody has touched
// recently.
const defaultRunnerIdleTTL = 15 * time.Minute

// CHANNEL_ATTACHED, NATS_URL, and IDLE_TTL. NATS_CREDS_PATH / NATS_CA_PATH
// are gated separately on o.NATSCredsMounted — they name the SubPath mount
// targets and must appear iff the per-session Secret actually carries the
// nats.creds/nats.ca keys (see buildRunnerVolumeMounts).
func buildRunnerEnv(o PodSpecOpts) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "AGENTSESSION_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "AGENTSESSION_NAME", Value: o.Session.Name},
		{Name: "OPERATOR_MEMORY_URL", Value: o.OperatorURL},
		{Name: "MEMORY_TOKEN_PATH", Value: "/var/run/agent/memory-token"},
		{Name: "LLM_API_KEY_PATH", Value: "/var/run/agent/llm-api-key"},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},

		// SpiceDB wiring: endpoint + insecure are value-copied from the
		// operator's resolved config (the operator process is the source
		// of truth — it reads SPICEDB_* from the install-time ConfigMap +
		// Secret in agentprimitives-system at startup via
		// spicedb.LoadEnvConfig). We can't use valueFrom
		// configMapKeyRef/secretKeyRef here because pods only see
		// resources in their own namespace, and runner pods live in the
		// session's namespace. The preshared token is NOT carried as an
		// env var: it rides in the per-session Secret and is mounted as a
		// file (see buildRunnerVolumeMounts); SPICEDB_TOKEN_PATH names
		// that file. The runner's own spicedb.LoadEnvConfig requires a
		// non-empty endpoint + resolved token at startup, so a missing
		// file fails the pod loudly.
		{Name: spicedb.EnvEndpoint, Value: o.SpiceDBEndpoint},
		{Name: spicedb.EnvTokenPath, Value: "/var/run/agent/spicedb-token"},
		{Name: spicedb.EnvInsecure, Value: strconv.FormatBool(o.SpiceDBInsecure)},
		{Name: "ARGS_HASH_KEY_PATH", Value: "/var/run/agent/args-hash-key"},
		{Name: "AUDIT_SIGNING_KEY_PATH", Value: "/var/run/agent/audit-signing-key"},
	}
	// Only when the operator actually knows the address. Stamping "" would
	// make "unset" and "known to be empty" indistinguishable to the runner,
	// which is the same fail-open shape as a defaulted cluster kind.
	if o.WebdBaseURL != "" {
		env = append(env, corev1.EnvVar{Name: externalurl.EnvWebdBaseURL, Value: o.WebdBaseURL})
	}
	// NATS_URL is set for EVERY session that has a bus address, not only
	// channel-attached ones: a headless (kubectl / `oap agent run`) session still
	// needs the bus to publish an approval interaction and await its decision, so
	// a human can drive it with `oap session approve` (which publishes the
	// decision on the session subject prefix, channel or not). Without it a
	// leakage / tool approval in a headless run fails closed with no way to ever
	// answer it. CHANNEL_ATTACHED + IDLE_TTL stay channel-gated — they turn on
	// the channel message plumbing and the await_user_message idle loop, which a
	// headless session has no use for.
	if o.NATSURL != "" {
		env = append(env, corev1.EnvVar{Name: "NATS_URL", Value: o.NATSURL})
	}
	if o.Session.Spec.InputChannel != nil {
		ttl := defaultRunnerIdleTTL
		if o.Class.Spec.Channels != nil && o.Class.Spec.Channels.IdleTTL.Duration > 0 {
			ttl = o.Class.Spec.Channels.IdleTTL.Duration
		}
		env = append(env,
			corev1.EnvVar{Name: "CHANNEL_ATTACHED", Value: "true"},
			corev1.EnvVar{Name: "IDLE_TTL", Value: ttl.String()},
		)
	}
	// NATS_CREDS_PATH / NATS_CA_PATH name the SubPath creds/ca mount
	// targets; they must be set iff buildRunnerVolumeMounts added those
	// mounts, i.e. iff the per-session Secret carries the keys.
	if o.NATSCredsMounted {
		env = append(env,
			corev1.EnvVar{Name: "NATS_CREDS_PATH", Value: "/var/run/agent/nats-creds"},
			corev1.EnvVar{Name: "NATS_CA_PATH", Value: "/var/run/agent/nats-ca"},
		)
	}
	// DEV/TEST: forward the relwrites user override from the operator
	// process's own environment to every runner pod, so a developer can
	// set it ONCE on the operator Deployment and have it apply to every
	// spawned runner without touching individual sessions. Empty /
	// unset = no-op. The operator itself never consults the value --
	// this line is its only reader on this side; the consumer is
	// internal/cmd/runner, which re-reads it from the pod env at startup.
	// See pkg/agent/tool/authz/relwrites/override_writer.go.
	if v := os.Getenv("DEV_RELWRITES_USER_EMAIL"); v != "" {
		env = append(env, corev1.EnvVar{Name: "DEV_RELWRITES_USER_EMAIL", Value: v})
	}
	// The dedicated derivation-validator model (fine_grained_info_leakage's
	// derive_tag) is a CLUSTER-WIDE config: one isolated validator serves every
	// runner. The runner reads PT_DERIVE_VALIDATOR_{PROVIDER,MODEL,API_KEY} at
	// startup (internal/cmd/runner/main.go) and leaves DeriveValidator nil —
	// derive_tag then unoffered — when they are unset. Forward them from the
	// operator's own environment, the same shape as DEV_RELWRITES_USER_EMAIL, so
	// an operator sets them ONCE on the operator Deployment (the API key from a
	// Secret via secretKeyRef) rather than per session. Empty/unset = no-op, so
	// every existing session is unaffected.
	//
	// PREVIEW LIMITATION: the API key rides as a plain env VALUE here, visible in
	// the runner pod spec — unlike the main LLM key, which is file-mounted via
	// LLM_API_KEY_PATH. Harden to a file mount (materialize into the per-session
	// Secret, add PT_DERIVE_VALIDATOR_API_KEY_PATH) before this leaves preview.
	for _, name := range []string{"PT_DERIVE_VALIDATOR_PROVIDER", "PT_DERIVE_VALIDATOR_MODEL", "PT_DERIVE_VALIDATOR_API_KEY"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: v})
		}
	}
	return env
}
