package agentsession_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/harness/apnative"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// TestPodSpecInjectsSpiceDBEnv pins the runner-pod wiring contract for
// SpiceDB. The operator is the single source of truth: it resolves the
// SPICEDB_* values via pkg/authz/spicedb.LoadEnvConfig at startup (against
// the install-time ConfigMap + Secret). SPICEDB_ENDPOINT / SPICEDB_INSECURE
// are passed onto each runner pod as plain env entries — we do NOT use
// valueFrom configMapKeyRef / secretKeyRef, because pods can only
// reference resources in their own namespace and runner pods live in the
// session's namespace while the SpiceDB ConfigMap + Secret live in
// agentprimitives-system. The preshared token is NOT carried as an env
// var: it rides in the per-session Secret and is mounted as a file the
// runner reads via SPICEDB_TOKEN_PATH.
// sessWithAPIKey returns an AgentSession with Status.EffectiveSettings pre-populated
// so BuildRunnerPod can read the apiKey without nil-derefing. Most podspec tests
// only care about env/volume names, not the apiKey contents.
func sessWithAPIKey(name string, secretName, secretKey string) *spiceboxv1alpha1.AgentSession {
	eff := &spiceboxv1alpha1.EffectiveSettings{
		Model: spiceboxv1alpha1.ModelConfig{
			APIKey: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: secretKey},
		},
	}
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{EffectiveSettings: eff},
	}
}

func TestPodSpecInjectsSpiceDBEnv(t *testing.T) {
	sess := sessWithAPIKey("spdb1", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:         sess,
		Class:           ac,
		Image:           "agentprimitives-runner:dev",
		ServiceAcct:     "spdb1-runner-sa",
		MemoryToken:     "spdb1-memory-token",
		OperatorURL:     "http://op:8082",
		NATSURL:         "nats://nats:4222",
		SpiceDBEndpoint: "spicebox-spicedb.agentprimitives-system.svc:50051",
		SpiceDBInsecure: true,
		Harness:         apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")

	byName := map[string]corev1.EnvVar{}
	for _, e := range pod.Spec.Containers[0].Env {
		byName[e.Name] = e
	}

	endpoint, ok := byName[spicedb.EnvEndpoint]
	require.True(t, ok, "%s must be injected", spicedb.EnvEndpoint)
	assert.Nil(t, endpoint.ValueFrom, "%s must be a plain value (no cross-ns valueFrom)", spicedb.EnvEndpoint)
	assert.Equal(t, "spicebox-spicedb.agentprimitives-system.svc:50051", endpoint.Value)

	insecure, ok := byName[spicedb.EnvInsecure]
	require.True(t, ok, "%s must be injected", spicedb.EnvInsecure)
	assert.Equal(t, "true", insecure.Value, "%s mirrors the operator's resolved config", spicedb.EnvInsecure)

	// The preshared token must never be a plaintext env var on the runner.
	_, hasTokenEnv := byName[spicedb.EnvToken]
	assert.False(t, hasTokenEnv, "%s must NOT be set on the runner pod (token is mounted as a file)", spicedb.EnvToken)

	// Instead, SPICEDB_TOKEN_PATH points at the mounted token file.
	tokenPath, ok := byName[spicedb.EnvTokenPath]
	require.True(t, ok, "%s must be injected", spicedb.EnvTokenPath)
	assert.Nil(t, tokenPath.ValueFrom, "%s must be a plain value", spicedb.EnvTokenPath)
	assert.Equal(t, "/var/run/agent/spicedb-token", tokenPath.Value)

	// And the per-session Secret's spicedb-token key is mounted at that path.
	var spicedbMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].MountPath == "/var/run/agent/spicedb-token" {
			spicedbMount = &pod.Spec.Containers[0].VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, spicedbMount, "runner must mount spicedb-token; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
	assert.Equal(t, "spicedb-token", spicedbMount.SubPath, "spicedb-token must project the per-session Secret key")
	assert.True(t, spicedbMount.ReadOnly, "spicedb-token mount must be read-only")
}

func TestPodSpecMountsArgsHashKey(t *testing.T) {
	sess := sessWithAPIKey("spdb1", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:         sess,
		Class:           ac,
		Image:           "agentprimitives-runner:dev",
		ServiceAcct:     "spdb1-runner-sa",
		MemoryToken:     "spdb1-memory-token",
		OperatorURL:     "http://op:8082",
		NATSURL:         "nats://nats:4222",
		SpiceDBEndpoint: "spicebox-spicedb.agentprimitives-system.svc:50051",
		SpiceDBInsecure: true,
		Harness:         apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")

	var keyPath *corev1.EnvVar
	for i := range pod.Spec.Containers[0].Env {
		if pod.Spec.Containers[0].Env[i].Name == "ARGS_HASH_KEY_PATH" {
			keyPath = &pod.Spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, keyPath, "runner env must carry ARGS_HASH_KEY_PATH")
	assert.Equal(t, "/var/run/agent/args-hash-key", keyPath.Value)

	var keyMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].MountPath == "/var/run/agent/args-hash-key" {
			keyMount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, keyMount, "runner must mount args-hash-key; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
	assert.Equal(t, "args-hash-key", keyMount.SubPath)
	assert.True(t, keyMount.ReadOnly)
}

func TestPodSpecMountsAuditSigningKey(t *testing.T) {
	sess := sessWithAPIKey("spdb1", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:         sess,
		Class:           ac,
		Image:           "agentprimitives-runner:dev",
		ServiceAcct:     "spdb1-runner-sa",
		MemoryToken:     "spdb1-memory-token",
		OperatorURL:     "http://op:8082",
		NATSURL:         "nats://nats:4222",
		SpiceDBEndpoint: "spicebox-spicedb.agentprimitives-system.svc:50051",
		SpiceDBInsecure: true,
		Harness:         apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")

	var keyPath *corev1.EnvVar
	for i := range pod.Spec.Containers[0].Env {
		if pod.Spec.Containers[0].Env[i].Name == "AUDIT_SIGNING_KEY_PATH" {
			keyPath = &pod.Spec.Containers[0].Env[i]
		}
	}
	require.NotNil(t, keyPath, "runner env must carry AUDIT_SIGNING_KEY_PATH")
	assert.Equal(t, "/var/run/agent/audit-signing-key", keyPath.Value)

	var keyMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].MountPath == "/var/run/agent/audit-signing-key" {
			keyMount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, keyMount, "runner must mount audit-signing-key; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
	assert.Equal(t, "audit-signing-key", keyMount.SubPath)
	assert.True(t, keyMount.ReadOnly)
}

func TestRunnerPodHasOwnerRef(t *testing.T) {
	sess := sessWithAPIKey("s1", "llm-creds", "api-key")
	sess.UID = "uid-s1"
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "anthropic", Name: "claude-opus-4-7",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       ac,
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "s1-runner-sa",
		MemoryToken: "s1-memory-token",
		OperatorURL: "http://spicebox-operator.agentprimitives-system.svc:8082",
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	require.Len(t, pod.OwnerReferences, 1, "expected exactly one owner reference")
	assert.Equal(t, sess.UID, pod.OwnerReferences[0].UID, "owner UID matches AgentSession")
	assert.Equal(t, corev1.RestartPolicyOnFailure, pod.Spec.RestartPolicy, "RestartPolicy")
	assert.Equal(t, "s1-runner-sa", pod.Spec.ServiceAccountName, "ServiceAccountName")
	require.Len(t, pod.Spec.Containers, 1, "expected exactly one container")
}

func TestRunnerPodMountsSecrets(t *testing.T) {
	sess := sessWithAPIKey("s1", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session: sess, Class: ac,
		Image: "x", ServiceAcct: "sa", MemoryToken: "memtok", OperatorURL: "http://op",
		Harness: apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		volNames[v.Name] = true
	}
	assert.True(t, volNames["memory-token"], "pod should mount the memory-token volume; volumes=%+v", volNames)
	assert.True(t, volNames["llm-api-key"], "pod should mount the llm-api-key volume; volumes=%+v", volNames)
}

func TestPodSpecChannelAttachedEnv(t *testing.T) {
	sess := sessWithAPIKey("ch1", "llm-creds", "api-key")
	sess.Spec.Class = "ac-ch"
	sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name:              "slack-ch",
		Kind:              "slack",
		Key:               "thread:C123:1234.5678",
		NATSSubjectPrefix: "ap.session.default.ch1",
	}
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
			Channels: &spiceboxv1alpha1.ChannelsConfig{
				IdleTTL: metav1.Duration{Duration: 30 * time.Second},
			},
		},
	}

	const wantNATSURL = "nats://spicebox-nats.agentprimitives-system.svc:4222"
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       ac,
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "ch1-runner-sa",
		MemoryToken: "ch1-memory-token",
		OperatorURL: "http://op:8082",
		NATSURL:     wantNATSURL,
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")

	envByName := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envByName[e.Name] = e.Value
	}

	assert.Equal(t, "true", envByName["CHANNEL_ATTACHED"], "CHANNEL_ATTACHED env")
	assert.Equal(t, wantNATSURL, envByName["NATS_URL"], "NATS_URL env")
	assert.Equal(t, "30s", envByName["IDLE_TTL"], "IDLE_TTL env from Channels.IdleTTL")
}

// TestPodSpecNATSCredsMounts asserts that the runner pod projects the
// per-session Secret's nats.creds / nats.ca keys at the fixed mount
// paths and exposes NATS_CREDS_PATH / NATS_CA_PATH iff PodSpecOpts.
// NATSCredsMounted is true — the controller sets that flag from the
// Secret's actual contents, NOT from InputChannel != nil. A
// channel-attached session whose Secret lacks the keys (operator
// started without a NATS identity) must NOT get the mounts; a SubPath
// mount of an absent key hangs the pod in ContainerCreating. The
// non-creds channel env (NATS_URL / CHANNEL_ATTACHED) stays gated on
// InputChannel != nil and is unaffected by the creds flag.
func TestPodSpecNATSCredsMounts(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}

	cases := []struct {
		name             string
		inputChannel     *spiceboxv1alpha1.ChannelBinding
		natsCredsMounted bool
		wantNATSMounts   bool
		wantChannelEnv   bool
	}{
		{
			name:             "channel-attached, Secret has creds: nats creds + CA mounted, paths exported",
			inputChannel:     &spiceboxv1alpha1.ChannelBinding{Name: "slack-ch", Kind: "slack"},
			natsCredsMounted: true,
			wantNATSMounts:   true,
			wantChannelEnv:   true,
		},
		{
			name:             "channel-attached, Secret lacks creds: no nats mounts, but channel env still present",
			inputChannel:     &spiceboxv1alpha1.ChannelBinding{Name: "slack-ch", Kind: "slack"},
			natsCredsMounted: false,
			wantNATSMounts:   false,
			wantChannelEnv:   true,
		},
		{
			name:             "kubectl session: no nats mounts, no channel env",
			inputChannel:     nil,
			natsCredsMounted: false,
			wantNATSMounts:   false,
			wantChannelEnv:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessWithAPIKey("s1", "llm-creds", "api-key")
			sess.Spec.InputChannel = tc.inputChannel
			pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
				Session:          sess,
				Class:            ac,
				Image:            "agentprimitives-runner:dev",
				ServiceAcct:      "s1-runner-sa",
				MemoryToken:      "s1-memory-token",
				OperatorURL:      "http://op:8082",
				NATSURL:          "nats://nats:4222",
				NATSCredsMounted: tc.natsCredsMounted,
				Harness:          apnative.Harness{},
			})
			require.NoError(t, err, "BuildRunnerPod")

			mountPaths := map[string]bool{}
			for _, m := range pod.Spec.Containers[0].VolumeMounts {
				mountPaths[m.MountPath] = true
			}
			envByName := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				envByName[e.Name] = e.Value
			}

			if tc.wantNATSMounts {
				assert.True(t, mountPaths["/var/run/agent/nats-creds"],
					"nats.creds must be mounted at /var/run/agent/nats-creds; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
				assert.True(t, mountPaths["/var/run/agent/nats-ca"],
					"nats.ca must be mounted at /var/run/agent/nats-ca; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
				assert.Equal(t, "/var/run/agent/nats-creds", envByName["NATS_CREDS_PATH"], "NATS_CREDS_PATH env")
				assert.Equal(t, "/var/run/agent/nats-ca", envByName["NATS_CA_PATH"], "NATS_CA_PATH env")
			} else {
				assert.False(t, mountPaths["/var/run/agent/nats-creds"],
					"must NOT mount nats.creds when NATSCredsMounted is false")
				assert.False(t, mountPaths["/var/run/agent/nats-ca"],
					"must NOT mount nats.ca when NATSCredsMounted is false")
				_, hasCredsEnv := envByName["NATS_CREDS_PATH"]
				_, hasCAEnv := envByName["NATS_CA_PATH"]
				assert.False(t, hasCredsEnv, "must NOT set NATS_CREDS_PATH when NATSCredsMounted is false")
				assert.False(t, hasCAEnv, "must NOT set NATS_CA_PATH when NATSCredsMounted is false")
			}

			// NATS_URL is set whenever the bus address is known (o.NATSURL != "",
			// true in every case here), independent of InputChannel — a headless
			// session needs the bus to drive approvals via `oap session approve`.
			// CHANNEL_ATTACHED stays gated on InputChannel (channel message
			// plumbing + the idle loop). Neither is affected by NATSCredsMounted.
			_, hasNATSURL := envByName["NATS_URL"]
			_, hasChannelAttached := envByName["CHANNEL_ATTACHED"]
			assert.True(t, hasNATSURL,
				"NATS_URL is set whenever the bus address is known, channel or not")
			assert.Equal(t, tc.wantChannelEnv, hasChannelAttached,
				"CHANNEL_ATTACHED presence is gated on InputChannel, not NATSCredsMounted")
		})
	}
}

// TestRunnerPodHardenedSecurityContext asserts that the runner pod is built
// with the same hardened security posture as the sandbox pod (pkg/platform/podspec/builder.go):
//   - pod-level SecurityContext: RunAsNonRoot, RunAsUser/Group/FSGroup=1000, SeccompProfile=RuntimeDefault
//   - EnableServiceLinks=false
//   - container SecurityContext: AllowPrivilegeEscalation=false, ReadOnlyRootFilesystem=true, Capabilities.Drop=["ALL"]
//   - a writable emptyDir volume mounted at /tmp
//
// AutomountServiceAccountToken is deliberately NOT disabled (the runner calls
// the Kubernetes API and requires its ServiceAccount token).
func TestRunnerPodHardenedSecurityContext(t *testing.T) {
	sess := sessWithAPIKey("harden1", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       ac,
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "harden1-runner-sa",
		MemoryToken: "harden1-memory-token",
		OperatorURL: "http://op:8082",
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")

	// --- Pod-level SecurityContext ---
	require.NotNil(t, pod.Spec.SecurityContext, "pod must have a SecurityContext")
	sc := pod.Spec.SecurityContext
	require.NotNil(t, sc.RunAsNonRoot, "RunAsNonRoot must be set")
	assert.True(t, *sc.RunAsNonRoot, "RunAsNonRoot must be true")
	require.NotNil(t, sc.RunAsUser, "RunAsUser must be set")
	assert.Equal(t, int64(1000), *sc.RunAsUser, "RunAsUser must be 1000")
	require.NotNil(t, sc.RunAsGroup, "RunAsGroup must be set")
	assert.Equal(t, int64(1000), *sc.RunAsGroup, "RunAsGroup must be 1000")
	require.NotNil(t, sc.FSGroup, "FSGroup must be set")
	assert.Equal(t, int64(1000), *sc.FSGroup, "FSGroup must be 1000")
	require.NotNil(t, sc.SeccompProfile, "SeccompProfile must be set")
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type, "SeccompProfile.Type must be RuntimeDefault")

	// --- EnableServiceLinks ---
	require.NotNil(t, pod.Spec.EnableServiceLinks, "EnableServiceLinks must be explicitly set")
	assert.False(t, *pod.Spec.EnableServiceLinks, "EnableServiceLinks must be false")

	// --- AutomountServiceAccountToken: must NOT be disabled (runner needs k8s API access) ---
	// Either nil (cluster default = true) or explicitly true are both acceptable.
	if pod.Spec.AutomountServiceAccountToken != nil {
		assert.True(t, *pod.Spec.AutomountServiceAccountToken,
			"AutomountServiceAccountToken must not be disabled — runner needs the k8s API")
	}

	// --- Container SecurityContext ---
	require.Len(t, pod.Spec.Containers, 1, "expected exactly one container")
	csc := pod.Spec.Containers[0].SecurityContext
	require.NotNil(t, csc, "container must have a SecurityContext")
	require.NotNil(t, csc.AllowPrivilegeEscalation, "AllowPrivilegeEscalation must be set")
	assert.False(t, *csc.AllowPrivilegeEscalation, "AllowPrivilegeEscalation must be false")
	require.NotNil(t, csc.ReadOnlyRootFilesystem, "ReadOnlyRootFilesystem must be set")
	assert.True(t, *csc.ReadOnlyRootFilesystem, "ReadOnlyRootFilesystem must be true")
	require.NotNil(t, csc.Capabilities, "Capabilities must be set")
	assert.Contains(t, csc.Capabilities.Drop, corev1.Capability("ALL"), "Capabilities.Drop must contain ALL")

	// --- /tmp emptyDir volume + mount ---
	var tmpVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "tmp" {
			tmpVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, tmpVol, "pod must have a 'tmp' volume; volumes=%+v", pod.Spec.Volumes)
	assert.NotNil(t, tmpVol.VolumeSource.EmptyDir, "'tmp' volume must be an emptyDir")

	var tmpMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].MountPath == "/tmp" {
			tmpMount = &pod.Spec.Containers[0].VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, tmpMount, "container must mount /tmp; mounts=%+v", pod.Spec.Containers[0].VolumeMounts)
	assert.Equal(t, "tmp", tmpMount.Name, "/tmp mount must use the 'tmp' volume")
}

func TestPodSpec_AppendsSidecars_AndRunnerEnvForEach(t *testing.T) {
	sess := sessWithAPIKey("s", "k", "k")
	cls := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-7", APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "k", Key: "k"}},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       cls,
		Image:       "runner:1",
		ServiceAcct: "sa",
		MemoryToken: "memtok",
		ResolvedSidecars: []spiceboxv1alpha1.ResolvedSidecarToolbox{{
			Name: "echo",
			Ref:  "echo",
			Port: 18080,
			Spec: spiceboxv1alpha1.SidecarToolboxSpec{
				Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/echo:v1"},
				Sandbox:   spiceboxv1alpha1.SidecarToolboxSandbox{Class: "sidecar-sandbox-default"},
				Transport: spiceboxv1alpha1.SidecarToolboxTransport{Port: 8080, Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
			},
		}},
		SidecarSecretName: func(ref string) string { return "sec-" + ref },
		Harness:           apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("got %d containers, want 2 (runner + 1 sidecar)", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[1].Name != "sidecar-echo" {
		t.Errorf("sidecar name=%q", pod.Spec.Containers[1].Name)
	}
	gotEnv := false
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "MCP_PORT_ECHO" && e.Value == "18080" {
			gotEnv = true
		}
	}
	if !gotEnv {
		t.Errorf("expected MCP_PORT_ECHO=18080 on runner, got %+v", pod.Spec.Containers[0].Env)
	}
}

// TestPodSpec_MixedRunModes asserts that BuildRunnerPod with a mix of resolved
// sidecars (one in-pod, one separate-pod) appends ONLY the in-pod one as a
// container, and the runner receives MCP_PORT_* env ONLY for the in-pod sidecar.
func TestPodSpec_MixedRunModes(t *testing.T) {
	sess := sessWithAPIKey("mixed", "k", "k")
	cls := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-7", APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "k", Key: "k"}},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       cls,
		Image:       "runner:1",
		ServiceAcct: "sa",
		MemoryToken: "memtok",
		ResolvedSidecars: []spiceboxv1alpha1.ResolvedSidecarToolbox{
			{
				Name:    "inpod-tool",
				Ref:     "inpod-tool",
				Port:    18080,
				RunMode: agentsession.RunModeInPod,
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/inpod:v1"},
					Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
				},
			},
			{
				Name:    "secret-tool",
				Ref:     "secret-tool",
				Port:    18081,
				RunMode: agentsession.RunModeSeparatePod,
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/secret:v1"},
					Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
				},
			},
		},
		SidecarSecretName: func(ref string) string { return "sec-" + ref },
		Harness:           apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")

	// Only the in-pod sidecar should be a container; runner + 1 = 2 total.
	require.Len(t, pod.Spec.Containers, 2, "expected runner + 1 in-pod sidecar; separate-pod must be excluded")
	containerNames := make([]string, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		containerNames[i] = c.Name
	}
	assert.Contains(t, containerNames, "sidecar-inpod-tool", "in-pod sidecar must be present as a container")
	assert.NotContains(t, containerNames, "sidecar-secret-tool", "separate-pod sidecar must NOT be a container in the agent pod")

	// Runner env must have MCP_PORT_* only for the in-pod sidecar.
	envByName := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envByName[e.Name] = e.Value
	}
	assert.Equal(t, "18080", envByName["MCP_PORT_INPOD_TOOL"], "runner must have MCP_PORT env for in-pod sidecar")
	_, hasSeparate := envByName["MCP_PORT_SECRET_TOOL"]
	assert.False(t, hasSeparate, "runner must NOT have MCP_PORT env for separate-pod sidecar")
}

func TestBuildRunnerPod_imagePullSecret(t *testing.T) {
	sess := sessWithAPIKey("ips1", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:         sess,
		Class:           ac,
		Image:           "agentprimitives-runner:dev",
		ServiceAcct:     "ips1-runner-sa",
		MemoryToken:     "ips1-memory-token",
		OperatorURL:     "http://op:8082",
		ImagePullSecret: "myreg-creds",
		Harness:         apnative.Harness{},
	})
	require.NoError(t, err)
	require.Len(t, pod.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "myreg-creds", pod.Spec.ImagePullSecrets[0].Name)
}

func TestPodSpecNoChannelEnvForKubectlSessions(t *testing.T) {
	// kubectl-driven session (no Channel in spec) must NOT get CHANNEL_ATTACHED.
	sess := sessWithAPIKey("plain", "llm-creds", "api-key")
	ac := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "llm-creds", Key: "api-key"},
			},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       ac,
		Image:       "agentprimitives-runner:dev",
		ServiceAcct: "plain-runner-sa",
		MemoryToken: "plain-memory-token",
		OperatorURL: "http://op:8082",
		NATSURL:     "nats://nats:4222",
		Harness:     apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	envByName := map[string]bool{}
	for _, e := range pod.Spec.Containers[0].Env {
		envByName[e.Name] = true
	}
	assert.False(t, envByName["CHANNEL_ATTACHED"], "kubectl-driven session must not set CHANNEL_ATTACHED")
	assert.False(t, envByName["IDLE_TTL"], "kubectl-driven session must not set IDLE_TTL")
	// NATS_URL IS set for a kubectl/headless session (when the bus address is
	// known): the runner connects to the bus solely to publish an approval
	// interaction and await its decision, so a human can drive it with
	// `oap session approve`. CHANNEL_ATTACHED stays off, so no channel message
	// plumbing or idle loop is turned on.
	assert.True(t, envByName["NATS_URL"], "headless session gets NATS_URL for the approval bus (channel env stays off)")
}

// TestPodSpec_InPodSidecarsDoNotGetTheRunnerServiceAccountToken pins the
// containment for the runner pod's ServiceAccount token.
//
// The pod deliberately leaves AutomountServiceAccountToken at its default
// (true) because the runner calls the Kubernetes API with it. In-pod
// SidecarToolbox containers are appended to the SAME pod, running images taken
// verbatim from SidecarToolbox.spec.source.image — arbitrary third-party MCP
// servers. Without suppression the ServiceAccount admission plugin injects the
// projected token into every one of them, handing a supply-chain-compromised
// image the full runner Role: patch on the AgentSession and its /status, get on
// the bound Channel's credentials Secret, get on the class AgentIdentity's
// credential Secrets.
//
// The suppression is the one the admission plugin honours: a container that
// already mounts at the API-token path does not get the token volume injected.
// The separate-pod and detector paths instead set automount=false on their own
// pods and are unaffected.
func TestPodSpec_InPodSidecarsDoNotGetTheRunnerServiceAccountToken(t *testing.T) {
	const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount"

	sess := sessWithAPIKey("satok", "k", "k")
	cls := &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-opus-4-7", APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "k", Key: "k"}},
		},
	}
	pod, err := agentsession.BuildRunnerPod(agentsession.PodSpecOpts{
		Session:     sess,
		Class:       cls,
		Image:       "runner:1",
		ServiceAcct: "sa",
		MemoryToken: "memtok",
		ResolvedSidecars: []spiceboxv1alpha1.ResolvedSidecarToolbox{
			{
				Name: "untrusted-a", Ref: "untrusted-a", Port: 18080, RunMode: agentsession.RunModeInPod,
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/third-party/a:v1"},
					Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
				},
			},
			{
				Name: "untrusted-b", Ref: "untrusted-b", Port: 18081, RunMode: agentsession.RunModeInPod,
				Spec: spiceboxv1alpha1.SidecarToolboxSpec{
					Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/third-party/b:v1"},
					Transport: spiceboxv1alpha1.SidecarToolboxTransport{Healthcheck: spiceboxv1alpha1.SidecarToolboxHealthcheck{Path: "/healthz", TimeoutSeconds: 30}},
				},
			},
		},
		SidecarSecretName: func(ref string) string { return "sec-" + ref },
		Harness:           apnative.Harness{},
	})
	require.NoError(t, err, "BuildRunnerPod")
	require.Len(t, pod.Spec.Containers, 3, "runner + 2 in-pod sidecars")

	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		volNames[v.Name] = true
	}

	for i, c := range pod.Spec.Containers {
		var mount *corev1.VolumeMount
		for j := range c.VolumeMounts {
			if c.VolumeMounts[j].MountPath == saTokenPath {
				mount = &c.VolumeMounts[j]
				break
			}
		}
		if i == 0 {
			assert.Nil(t, mount, "the runner container must NOT shadow the SA token path; it needs the token")
			continue
		}
		require.NotNil(t, mount,
			"sidecar %q must shadow %s so the ServiceAccount admission plugin skips it; mounts=%+v",
			c.Name, saTokenPath, c.VolumeMounts)
		assert.True(t, volNames[mount.Name],
			"sidecar %q shadows the SA token path with volume %q, which must be declared on the pod", c.Name, mount.Name)
	}
}
