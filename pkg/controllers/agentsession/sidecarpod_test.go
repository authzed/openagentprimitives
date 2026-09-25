// Pure-function tests for the separate per-session sidecar pod builder:
// SidecarPodName, BuildSidecarPod (container reuse, secret injection by deliver
// mode, ownerref, probe), and secretTokenHash.
package agentsession_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// sessForPod returns a minimal AgentSession with a UID set so ownerref
// assertions are meaningful.
func sessForPod(name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("sess-uid-123"),
		},
	}
}

// separatePodToolbox returns a resolved separate-pod toolbox with one
// secretInput delivered per the given deliver spec.
func separatePodToolbox(ref, deliver, from, inputName string) spiceboxv1alpha1.ResolvedSidecarToolbox {
	return spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name:    ref,
		Ref:     ref,
		Port:    18080,
		RunMode: agentsession.RunModeSeparatePod,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Source:    spiceboxv1alpha1.SidecarToolboxSource{Image: "ghcr.io/x/" + ref + ":v1"},
			Transport: spiceboxv1alpha1.SidecarToolboxTransport{Port: 18080},
			SecretInputs: []spiceboxv1alpha1.SidecarToolboxSecretInput{
				{Name: inputName, Deliver: deliver, From: from},
			},
		},
	}
}

func TestSidecarPodName(t *testing.T) {
	sess := sessForPod("s1")
	assert.Equal(t, "s1-sidecar-k8s", agentsession.SidecarPodName(sess, "k8s"))
}

func TestSecretTokenHash(t *testing.T) {
	want := sha256.Sum256([]byte("hello"))
	assert.Equal(t, hex.EncodeToString(want[:]), agentsession.SecretTokenHash([]byte("hello")))
	// Distinct inputs produce distinct hashes.
	assert.NotEqual(t,
		agentsession.SecretTokenHash([]byte("a")),
		agentsession.SecretTokenHash([]byte("b")),
		"different secret values must hash differently")
}

func TestBuildSidecarPod_ImageSource_EnvDeliver(t *testing.T) {
	sess := sessForPod("s1")
	rt := separatePodToolbox("k8s", "env", "kubeconfig", "KUBECONFIG")

	pod, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", nil, "", "", "")
	require.NoError(t, err, "BuildSidecarPod must succeed for image source")

	// Name.
	assert.Equal(t, "s1-sidecar-k8s", pod.Name)
	assert.Equal(t, "default", pod.Namespace)

	// Owner ref to the AgentSession (controller + block-owner-deletion + UID).
	require.Len(t, pod.OwnerReferences, 1, "exactly one owner ref")
	owner := pod.OwnerReferences[0]
	assert.Equal(t, "AgentSession", owner.Kind)
	assert.Equal(t, "s1", owner.Name)
	assert.Equal(t, types.UID("sess-uid-123"), owner.UID)
	require.NotNil(t, owner.Controller)
	assert.True(t, *owner.Controller, "owner ref Controller=true")
	require.NotNil(t, owner.BlockOwnerDeletion)
	assert.True(t, *owner.BlockOwnerDeletion, "owner ref BlockOwnerDeletion=true")

	// Label identifies session + ref.
	assert.Equal(t, "s1", pod.Labels["agentprimitives.authzed.com/agentsession"])
	assert.Equal(t, "k8s", pod.Labels["agentprimitives.authzed.com/sidecartoolbox"])

	// Long-running MCP wants OnFailure restart.
	assert.Equal(t, corev1.RestartPolicyOnFailure, pod.Spec.RestartPolicy)

	// SA token must not be automounted: a user-supplied sidecar has no business
	// holding an apiserver credential.
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken, "sidecar pod must not mount an SA token")

	// The upstream-credential Secret (spec.upstreamAuth → TS_AUTHKEY etc.) is
	// envFrom'd into the SIDECAR container so a separate-pod sidecar carries its
	// own upstream cred. Regression: without this the tailscale tunnel never came
	// up and cluster tools failed with a DNS lookup error.
	require.Len(t, pod.Spec.Containers, 1, "one sidecar container")
	var hasCredEnvFrom bool
	for _, ef := range pod.Spec.Containers[0].EnvFrom {
		if ef.SecretRef != nil && ef.SecretRef.Name == "s1-toolbox-kube-mcp-creds" {
			hasCredEnvFrom = true
		}
	}
	assert.True(t, hasCredEnvFrom, "sidecar container must envFrom the upstream-credential Secret")

	// One container with the toolbox image + hardened sec ctx + probe.
	require.Len(t, pod.Spec.Containers, 1, "single sidecar container")
	c := pod.Spec.Containers[0]
	assert.Equal(t, "ghcr.io/x/k8s:v1", c.Image, "image from source")
	require.NotNil(t, c.SecurityContext, "hardened security context")
	require.NotNil(t, c.SecurityContext.RunAsNonRoot)
	assert.True(t, *c.SecurityContext.RunAsNonRoot)
	require.NotNil(t, c.StartupProbe, "startup probe present")
	require.NotNil(t, c.StartupProbe.HTTPGet)
	assert.Equal(t, int32(18080), c.StartupProbe.HTTPGet.Port.IntVal, "probe hits rt.Port")

	// MCP_PORT env set.
	assert.True(t, hasEnv(c.Env, "MCP_PORT", "18080"), "MCP_PORT env set to rt.Port")

	// deliver:env → an env var named the input Name, valueFrom the
	// per-session secret-output Secret key From.
	var found *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "KUBECONFIG" {
			found = &c.Env[i]
			break
		}
	}
	require.NotNil(t, found, "secret input env var KUBECONFIG present")
	require.NotNil(t, found.ValueFrom, "env var sourced from secret")
	require.NotNil(t, found.ValueFrom.SecretKeyRef)
	assert.Equal(t, "s1-secret-outputs", found.ValueFrom.SecretKeyRef.Name, "secret-output Secret name")
	assert.Equal(t, "kubeconfig", found.ValueFrom.SecretKeyRef.Key, "Secret key == From")

	// No file mount for env-delivery.
	for _, vm := range c.VolumeMounts {
		assert.NotEqual(t, "/root/.kube/config", vm.MountPath, "no file mount for env deliver")
	}
}

func TestBuildSidecarPod_FileDeliver_MountsSecretKey(t *testing.T) {
	sess := sessForPod("s2")
	rt := separatePodToolbox("k8s", "file:/root/.kube/config", "kubeconfig", "KUBECONFIG")

	pod, err := agentsession.BuildSidecarPod(sess, rt, "s2-secret-outputs", "", nil, "", "", "")
	require.NoError(t, err)

	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]

	// A volume projecting the secret-output Secret key.
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Secret != nil && pod.Spec.Volumes[i].Secret.SecretName == "s2-secret-outputs" {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "a volume projects the secret-output Secret")
	require.Len(t, vol.Secret.Items, 1, "one key projected")
	assert.Equal(t, "kubeconfig", vol.Secret.Items[0].Key, "projects the From key")

	// A mount of that volume at the deliver path's directory, with the
	// filename ending in config.
	var mount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == vol.Name {
			mount = &c.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, mount, "the secret volume is mounted into the container")
	assert.Contains(t, mount.MountPath, "/root/.kube", "mount targets the deliver path dir")

	// No KUBECONFIG env var for file-delivery (the value is a file, not an env).
	assert.False(t, hasEnvName(c.Env, "KUBECONFIG"), "no env var for file deliver")
}

// TestBuildSidecarPod_ConfigEnv proves rt.Spec.Config reaches the
// separate-pod container's AP_SIDECAR_CONFIG env var via the same shared
// buildSidecarContainer helper the in-pod path uses (BuildSidecarContainers,
// covered in sidecars_test.go).
func TestBuildSidecarPod_ConfigEnv(t *testing.T) {
	sess := sessForPod("s1")
	rt := separatePodToolbox("k8s", "env", "kubeconfig", "KUBECONFIG")
	rt.Spec.Config = `{"base_url":"https://example.com"}`

	pod, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", nil, "", "", "")
	require.NoError(t, err, "BuildSidecarPod must succeed for image source")

	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]
	assert.True(t, hasEnv(c.Env, "AP_SIDECAR_CONFIG", `{"base_url":"https://example.com"}`),
		"AP_SIDECAR_CONFIG env set from rt.Spec.Config, got %+v", c.Env)
}

// TestBuildSidecarPod_WorkshopIdentity proves that a non-nil workshop
// identity puts the pod under the workshop ServiceAccount with a projected
// token and the workshop bearer secret envFrom'd — IN ADDITION to the
// existing cred-secret envFrom, not instead of it.
func TestBuildSidecarPod_WorkshopIdentity(t *testing.T) {
	sess := sessForPod("s1")
	rt := separatePodToolbox("k8s", "env", "kubeconfig", "KUBECONFIG")
	identity := &spiceboxv1alpha1.WorkshopSidecarIdentity{
		ServiceAccount: "s1-workshop-sa",
		TokenSecret:    "s1-workshop-token",
	}

	const operatorURL = "http://spicebox-operator.agentprimitives-system.svc:8082"
	const workshopNamespace = "ws-abc123def456"
	const trustedImageRegistry = "registry.example.test"
	pod, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", identity, operatorURL, workshopNamespace, trustedImageRegistry)
	require.NoError(t, err, "BuildSidecarPod must succeed with a workshop identity")

	assert.Equal(t, "s1-workshop-sa", pod.Spec.ServiceAccountName, "pod runs under the workshop SA")

	// AutomountServiceAccountToken stays false: the projection is explicit,
	// the automount stays off.
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken, "automount stays off even with a workshop identity")

	// A projected volume named workshop-sa-token with a ServiceAccountToken
	// source, Path "token", ExpirationSeconds 3600.
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workshop-sa-token" {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "a workshop-sa-token volume must be present")
	require.NotNil(t, vol.Projected, "workshop-sa-token volume must be a projected volume")
	// TWO projected sources: the SA token AND the cluster CA (kube-root-ca.crt).
	// The workshop image builds its API client from THIS mount alone — the pod
	// has automount off, so rest.InClusterConfig's default-path token/CA are
	// absent and the client must read both the token and ca.crt from here.
	require.Len(t, vol.Projected.Sources, 2, "the SA token AND the cluster CA")
	var tokenSrc *corev1.ServiceAccountTokenProjection
	var caSrc *corev1.ConfigMapProjection
	for i := range vol.Projected.Sources {
		if s := vol.Projected.Sources[i].ServiceAccountToken; s != nil {
			tokenSrc = s
		}
		if s := vol.Projected.Sources[i].ConfigMap; s != nil {
			caSrc = s
		}
	}
	require.NotNil(t, tokenSrc, "projected source includes a ServiceAccountToken")
	require.NotNil(t, caSrc, "projected source includes the cluster-CA ConfigMap")
	assert.Equal(t, "kube-root-ca.crt", caSrc.Name, "the cluster CA comes from the auto-created kube-root-ca.crt ConfigMap")
	require.Len(t, caSrc.Items, 1)
	assert.Equal(t, "ca.crt", caSrc.Items[0].Key)
	assert.Equal(t, "ca.crt", caSrc.Items[0].Path, "ca.crt lands next to token at the workshop mount path")
	assert.Equal(t, "token", tokenSrc.Path)
	require.NotNil(t, tokenSrc.ExpirationSeconds)
	assert.Equal(t, int64(3600), *tokenSrc.ExpirationSeconds)

	// The container mounts it read-only at the fixed workshop path.
	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]
	var mount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "workshop-sa-token" {
			mount = &c.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, mount, "the workshop-sa-token volume must be mounted into the container")
	assert.Equal(t, "/var/run/secrets/workshop/serviceaccount", mount.MountPath)
	assert.True(t, mount.ReadOnly, "workshop token mount is read-only")

	// EnvFrom carries BOTH the cred secret and the workshop token secret.
	var hasCredEnvFrom, hasWorkshopEnvFrom bool
	for _, ef := range c.EnvFrom {
		if ef.SecretRef == nil {
			continue
		}
		switch ef.SecretRef.Name {
		case "s1-toolbox-kube-mcp-creds":
			hasCredEnvFrom = true
		case "s1-workshop-token":
			hasWorkshopEnvFrom = true
		}
	}
	assert.True(t, hasCredEnvFrom, "the existing cred-secret envFrom must still be present")
	assert.True(t, hasWorkshopEnvFrom, "the workshop token secret must be envFrom'd in addition")

	// internal/cmd/workshop's five platform env vars: OPERATOR_MEMORY_URL (so
	// the sidecar can reach the operator with the bearer above) and the
	// workshop identity (WORKSHOP_NAMESPACE/WORKSHOP_ID both name W;
	// WORKSHOP_SESSION_NAMESPACE/WORKSHOP_SESSION_NAME name the builder
	// AgentSession B/X, not W).
	assert.True(t, hasEnv(c.Env, "OPERATOR_MEMORY_URL", operatorURL), "OPERATOR_MEMORY_URL must be set")
	assert.True(t, hasEnv(c.Env, "WORKSHOP_NAMESPACE", workshopNamespace), "WORKSHOP_NAMESPACE must be W")
	assert.True(t, hasEnv(c.Env, "WORKSHOP_SESSION_NAMESPACE", sess.Namespace), "WORKSHOP_SESSION_NAMESPACE must be the builder session's namespace")
	assert.True(t, hasEnv(c.Env, "WORKSHOP_SESSION_NAME", sess.Name), "WORKSHOP_SESSION_NAME must be the builder session's name")
	assert.True(t, hasEnv(c.Env, "WORKSHOP_ID", workshopNamespace), "WORKSHOP_ID must also be W")
	assert.True(t, hasEnv(c.Env, "WORKSHOP_TRUSTED_IMAGE_REGISTRY", trustedImageRegistry),
		"WORKSHOP_TRUSTED_IMAGE_REGISTRY must be set so inventory can report registry-qualified refs")
}

// TestBuildSidecarPod_WorkshopIdentity_EmptyTrustedRegistryOmitsEnv proves an
// empty trustedImageRegistry (a local/desktop install with no trusted
// registry) omits WORKSHOP_TRUSTED_IMAGE_REGISTRY entirely — never sent as
// an empty-valued env var — even though this IS a workshop-identity pod.
func TestBuildSidecarPod_WorkshopIdentity_EmptyTrustedRegistryOmitsEnv(t *testing.T) {
	sess := sessForPod("s1")
	rt := separatePodToolbox("k8s", "env", "kubeconfig", "KUBECONFIG")
	identity := &spiceboxv1alpha1.WorkshopSidecarIdentity{
		ServiceAccount: "s1-workshop-sa",
		TokenSecret:    "s1-workshop-token",
	}

	pod, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", identity,
		"http://spicebox-operator.agentprimitives-system.svc:8082", "ws-abc123def456", "")
	require.NoError(t, err, "BuildSidecarPod must succeed with a workshop identity and no trusted registry")

	require.Len(t, pod.Spec.Containers, 1)
	assert.False(t, hasEnvName(pod.Spec.Containers[0].Env, "WORKSHOP_TRUSTED_IMAGE_REGISTRY"),
		"an empty trustedImageRegistry must omit the env var entirely, not send it as \"\"")
}

// TestBuildSidecarPod_NonWorkshopSidecar_NeverCarriesTrustedRegistry is the
// negative case that matters: a non-workshop sidecar (identity == nil) must
// never learn the cluster's trusted registry, even when the operator has one
// configured and passes it in. WORKSHOP_TRUSTED_IMAGE_REGISTRY is read only
// inside BuildSidecarPod's identity-non-nil branch, so a caller cannot leak it
// onto a user-supplied sidecar by accident.
func TestBuildSidecarPod_NonWorkshopSidecar_NeverCarriesTrustedRegistry(t *testing.T) {
	sess := sessForPod("s1")
	rt := separatePodToolbox("k8s", "env", "kubeconfig", "KUBECONFIG")

	pod, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", nil,
		"http://spicebox-operator.agentprimitives-system.svc:8082", "ws-abc123def456", "registry.example.test")
	require.NoError(t, err, "BuildSidecarPod must succeed with a nil identity")

	require.Len(t, pod.Spec.Containers, 1)
	assert.False(t, hasEnvName(pod.Spec.Containers[0].Env, "WORKSHOP_TRUSTED_IMAGE_REGISTRY"),
		"a non-workshop sidecar (nil identity) must never carry WORKSHOP_TRUSTED_IMAGE_REGISTRY, even when the operator has a trusted registry configured")
}

// TestBuildSidecarPod_NilIdentityIsByteIdenticalToBefore proves the seam is
// invisible to every non-workshop sidecar: a nil identity must produce a pod
// identical (DeepEqual) to one built by a call with no identity parameter at
// all — no SA name, no workshop volume, no extra EnvFrom, and — the plan-2
// touch this test now also guards — NONE of the six platform env vars, even
// when operatorURL/workshopNamespace/trustedImageRegistry are non-empty.
// Those three are read only inside the identity branch, so passing them
// alongside a nil identity below must have zero effect: proof that a caller
// cannot leak OPERATOR_MEMORY_URL/WORKSHOP_* onto a non-workshop sidecar by
// accident.
func TestBuildSidecarPod_NilIdentityIsByteIdenticalToBefore(t *testing.T) {
	sess := sessForPod("s1")
	rt := separatePodToolbox("k8s", "env", "kubeconfig", "KUBECONFIG")

	before, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", nil, "", "", "")
	require.NoError(t, err)
	after, err := agentsession.BuildSidecarPod(sess, rt, "s1-secret-outputs", "s1-toolbox-kube-mcp-creds", nil,
		"http://spicebox-operator.agentprimitives-system.svc:8082", "ws-abc123def456", "registry.example.test")
	require.NoError(t, err)

	assert.Equal(t, before, after, "a nil identity must produce a pod byte-identical to today's — the seam must be invisible to every non-workshop sidecar, even with operatorURL/workshopNamespace/trustedImageRegistry set")
	assert.Empty(t, after.Spec.ServiceAccountName, "no SA name without an identity")
	for _, v := range after.Spec.Volumes {
		assert.NotEqual(t, "workshop-sa-token", v.Name, "no workshop volume without an identity")
	}
	for _, ef := range after.Spec.Containers[0].EnvFrom {
		if ef.SecretRef != nil {
			assert.NotEqual(t, "s1-workshop-token", ef.SecretRef.Name)
		}
	}
	require.Len(t, after.Spec.Containers, 1)
	for _, name := range []string{"OPERATOR_MEMORY_URL", "WORKSHOP_NAMESPACE", "WORKSHOP_SESSION_NAMESPACE", "WORKSHOP_SESSION_NAME", "WORKSHOP_ID", "WORKSHOP_TRUSTED_IMAGE_REGISTRY"} {
		assert.False(t, hasEnvName(after.Spec.Containers[0].Env, name), "a nil identity must carry no %s", name)
	}
}

func hasEnv(env []corev1.EnvVar, name, val string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == val {
			return true
		}
	}
	return false
}

func hasEnvName(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

// TestBuildSidecarPod_TerminationMessagePolicy_FallbackToLogs pins the policy
// that makes a crashed sidecar's REAL cause readable from pod status.
//
// The kubelet's CrashLoopBackOff waiting-message is only "back-off Ns
// restarting failed container=…", which names no cause. With
// FallbackToLogsOnError the kubelet copies the container's log tail into
// lastState.terminated.message whenever the termination-message file is empty
// and the container exited non-zero — so the operator reads the actual error
// (e.g. an MCP server rejecting its startup config) off the Pod it already
// watches, with no pods/log RBAC and no log streaming. Under the default
// (File) policy the message is empty for every image that does not write
// /dev/termination-log, which is all of them.
func TestBuildSidecarPod_TerminationMessagePolicy_FallbackToLogs(t *testing.T) {
	sess := sessForPod("sess-a")
	rt := separatePodToolbox("k8s-mcp", "file:/var/run/sre/kubeconfig", "kubeconfig", "kubeconfig")

	pod, err := agentsession.BuildSidecarPod(sess, rt, "so-secret", "cred-secret", nil, "", "", "")
	require.NoError(t, err, "BuildSidecarPod must succeed for a file-deliver toolbox")
	require.Len(t, pod.Spec.Containers, 1, "separate-pod sidecar is single-container")

	assert.Equal(t, corev1.TerminationMessageFallbackToLogsOnError,
		pod.Spec.Containers[0].TerminationMessagePolicy,
		"sidecar container must fall back to the log tail so a crash cause reaches status")
}

// podCrashed builds a Pod whose single container is Waiting with the given
// reason AND carries a LastTerminationState — the shape the kubelet produces
// for a container that ran, exited non-zero, and is now backing off.
func podCrashed(reason, waitMsg, termMsg string, exitCode int32) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "c",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: waitMsg}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exitCode,
			Reason:   "Error",
			Message:  termMsg,
		}},
	}}}}
}

// TestSidecarPodFailure classifies a separate-pod sidecar's Pod status into the
// operator-owned status record the runner later surfaces to the agent + user.
//
// The load-bearing case is the first one: for a crash-looping container the
// kubelet's WAITING message is content-free ("back-off … restarting failed
// container="), while the actionable cause lives in the terminated message
// (the log tail, courtesy of FallbackToLogsOnError). A classifier that
// preferred the waiting message would surface a failure the user cannot act
// on — which is the whole bug this fixes.
func TestSidecarPodFailure(t *testing.T) {
	const logTail = `no cluster match for "demo-cluster.abc123" in region "us-east-1"`

	cases := []struct {
		name         string
		pod          *corev1.Pod
		wantFailure  bool
		wantReason   string
		wantMessage  string
		wantExitCode *int32
	}{
		{
			name:         "crash loop with log tail: prefers the terminated message over the useless kubelet back-off text",
			pod:          podCrashed("CrashLoopBackOff", "back-off 2m40s restarting failed container=sidecar-k8s-mcp", logTail, 1),
			wantFailure:  true,
			wantReason:   "CrashLoopBackOff",
			wantMessage:  logTail,
			wantExitCode: ptr.To(int32(1)),
		},
		{
			name:        "crash loop with no terminated message: falls back to the kubelet waiting message",
			pod:         podCrashed("CrashLoopBackOff", "back-off 10s restarting failed container=sidecar-x", "", 2),
			wantFailure: true,
			wantReason:  "CrashLoopBackOff",
			// Empty log tail (container wrote nothing) must not yield an empty
			// message — the user still needs SOMETHING naming the failure.
			wantMessage:  "back-off 10s restarting failed container=sidecar-x",
			wantExitCode: ptr.To(int32(2)),
		},
		{
			name:        "ImagePullBackOff never ran, so there is no exit code",
			pod:         podWithWaiting("ImagePullBackOff", `Back-off pulling image "ghcr.io/x:latest"`),
			wantFailure: true,
			wantReason:  "ImagePullBackOff",
			wantMessage: `Back-off pulling image "ghcr.io/x:latest"`,
		},
		{name: "ContainerCreating is transient: no failure recorded", pod: podWithWaiting("ContainerCreating", "")},
		{name: "first ErrImagePull (pre-backoff) is transient: no failure recorded", pod: podWithWaiting("ErrImagePull", "pull failed")},
		{name: "Running container: no failure recorded", pod: podWithWaiting("", "")},
		{name: "no container statuses yet: no failure recorded", pod: &corev1.Pod{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentsession.SidecarPodFailureForTest(tc.pod)

			if !tc.wantFailure {
				assert.Nil(t, got, "transient/healthy pod must not record a failure")
				return
			}

			require.NotNil(t, got, "terminal pod must record a failure")
			assert.Equal(t, tc.wantReason, got.Reason, "reason")
			assert.Equal(t, tc.wantMessage, got.Message, "message must carry the actionable cause")
			assert.Equal(t, tc.wantExitCode, got.ExitCode, "exit code")
		})
	}
}

// podWithWaiting builds a Pod whose single container is Waiting with the given
// reason/message (or, for an empty reason, a benign Running container).
func podWithWaiting(reason, message string) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "c"}
	if reason == "" {
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	} else {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}
	}
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}}}
}

// TestTerminalPodWaitReason verifies the helper that lets the detector loop
// fail closed in seconds (vs the 5m deadline) when a pod is wedged in a state
// it will not recover from — and, crucially, that it does NOT fire for benign
// transient states, which would prematurely fail a still-starting detector.
func TestTerminalPodWaitReason(t *testing.T) {
	cases := []struct {
		name       string
		pod        *corev1.Pod
		wantOK     bool
		wantReason string
	}{
		{name: "ImagePullBackOff is terminal (the pirate-bot case)", pod: podWithWaiting("ImagePullBackOff", `Back-off pulling image "ghcr.io/x:latest"`), wantOK: true, wantReason: "ImagePullBackOff"},
		{name: "CrashLoopBackOff is terminal", pod: podWithWaiting("CrashLoopBackOff", "back-off restarting"), wantOK: true, wantReason: "CrashLoopBackOff"},
		{name: "CreateContainerConfigError is terminal", pod: podWithWaiting("CreateContainerConfigError", "secret not found"), wantOK: true, wantReason: "CreateContainerConfigError"},
		{name: "InvalidImageName is terminal", pod: podWithWaiting("InvalidImageName", "bad ref"), wantOK: true, wantReason: "InvalidImageName"},
		{name: "ContainerCreating is transient, not terminal", pod: podWithWaiting("ContainerCreating", ""), wantOK: false},
		{name: "PodInitializing is transient, not terminal", pod: podWithWaiting("PodInitializing", ""), wantOK: false},
		{name: "first ErrImagePull (pre-backoff) is not treated as terminal", pod: podWithWaiting("ErrImagePull", "pull failed"), wantOK: false},
		{name: "Running container yields no terminal reason", pod: podWithWaiting("", ""), wantOK: false},
		{name: "no container statuses yet yields no terminal reason", pod: &corev1.Pod{}, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg, ok := agentsession.TerminalPodWaitReasonForTest(tc.pod)
			assert.Equal(t, tc.wantOK, ok, "terminal?")
			if tc.wantOK {
				assert.Equal(t, tc.wantReason, reason, "reason")
				assert.NotEmpty(t, msg, "terminal reasons should carry the kubelet message for the user-facing failure")
			}
		})
	}
}
