package cosidecar

import (
	"testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildContainer_imageSourceHardened(t *testing.T) {
	c, vols, err := BuildContainer(Spec{
		Ref:    "det",
		Image:  "example.com/det@sha256:abc",
		Port:   8919,
		Health: Healthcheck{Path: "/healthz", TimeoutSeconds: 20},
	})
	require.NoError(t, err)
	// Two emptyDirs are always added: a writable /tmp (read-only rootfs needs
	// scratch) and the ServiceAccount-token shadow. Asserted by name rather
	// than by index/count so adding a third does not break this test.
	tmp := volumeNamed(t, vols, "tmp-det")
	require.NotNil(t, tmp.EmptyDir, "/tmp must be a writable emptyDir")
	sa := volumeNamed(t, vols, "no-sa-token-det")
	require.NotNil(t, sa.EmptyDir, "the SA-token shadow must be an emptyDir")
	assert.Equal(t, "sidecar-det", c.Name)
	assert.Equal(t, "example.com/det@sha256:abc", c.Image)
	require.NotNil(t, c.SecurityContext)
	assert.True(t, *c.SecurityContext.RunAsNonRoot)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, c.StartupProbe)
	assert.Equal(t, "/healthz", c.StartupProbe.HTTPGet.Path)
}

func TestBuildNetworkPolicy_deniedEgress_hasNoEgressRules(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns"}}
	np := BuildNetworkPolicy(sess, Spec{Ref: "det", Port: 8919, Egress: EgressPolicy{Denied: true}})
	require.NotNil(t, np)
	assert.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)
	assert.Empty(t, np.Spec.Egress, "denied egress ⇒ zero egress rules = deny-all")
	require.Len(t, np.Spec.Ingress, 1, "ingress: runner-only on the detector port")
	require.Len(t, np.Spec.Ingress[0].Ports, 1)
	assert.Equal(t, int32(8919), np.Spec.Ingress[0].Ports[0].Port.IntVal)
	_ = corev1.ProtocolTCP
}

func TestBuildContainer_inlineSource(t *testing.T) {
	c, vols, err := BuildContainer(Spec{
		Ref:  "inline-det",
		Port: 9090,
		Inline: &InlineSource{
			BaseImage:     "example.com/base:latest",
			Entrypoint:    []string{"/entrypoint.sh"},
			ConfigMapName: "det-script",
			ConfigMapKey:  "run.sh",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "sidecar-inline-det", c.Name)
	assert.Equal(t, "example.com/base:latest", c.Image)
	assert.Equal(t, []string{"/entrypoint.sh"}, c.Command)
	require.Len(t, vols, 3, "inline source volume + writable /tmp + SA-token shadow")
	require.NotNil(t, vols[0].ConfigMap)
	assert.Equal(t, "det-script", vols[0].ConfigMap.Name)
	require.NotNil(t, volumeNamed(t, vols, "tmp-inline-det").EmptyDir)
	require.NotNil(t, volumeNamed(t, vols, "no-sa-token-inline-det").EmptyDir)
	assert.Equal(t, []string{"/app", "/tmp", saTokenMountPath}, mountPaths(c),
		"/app source mount + /tmp scratch + the SA-token shadow")
}

// volumeNamed returns the volume with the given name, failing the test when it
// is absent.
func volumeNamed(t *testing.T, vols []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, v := range vols {
		if v.Name == name {
			return v
		}
	}
	require.FailNowf(t, "volume not found", "no volume named %q in %+v", name, vols)
	return corev1.Volume{}
}

// mountPaths returns c's volumeMount paths in declaration order.
func mountPaths(c corev1.Container) []string {
	paths := make([]string, 0, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		paths = append(paths, m.MountPath)
	}
	return paths
}

// TestBuildContainer_shadowsTheServiceAccountTokenPath pins the containment
// that keeps a third-party MCP image out of the runner's Kubernetes identity.
//
// An in-pod cosidecar shares the RUNNER's pod, which leaves
// AutomountServiceAccountToken on because the runner needs the token. Without
// a mount at DefaultAPITokenMountPath the ServiceAccount admission plugin
// injects that token into the sidecar too, giving an arbitrary user-supplied
// image the runner Role (patch on the AgentSession + /status, get on the
// Channel's and AgentIdentity's credential Secrets).
func TestBuildContainer_shadowsTheServiceAccountTokenPath(t *testing.T) {
	c, vols, err := BuildContainer(Spec{Ref: "det", Image: "example.com/det:latest", Port: 8919})
	require.NoError(t, err)

	var shadow *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == saTokenMountPath {
			shadow = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, shadow,
		"a cosidecar must shadow %s; mounts=%+v", saTokenMountPath, c.VolumeMounts)
	assert.True(t, shadow.ReadOnly, "the shadow is never written to")
	require.NotNil(t, volumeNamed(t, vols, shadow.Name).EmptyDir,
		"the shadow must be an emptyDir declared alongside the container")
}

// TestBuildContainer_writableTmp asserts every cosidecar container gets a
// writable /tmp emptyDir alongside its read-only rootfs. Without it the
// Python/torch prompt-injection detector crash-loops with "No usable temporary
// directory found" before it can serve. The volume name is per-ref so multiple
// in-pod sidecars sharing the runner pod do not collide.
func TestBuildContainer_writableTmp(t *testing.T) {
	c, vols, err := BuildContainer(Spec{Ref: "det", Image: "example.com/det:latest", Port: 8919})
	require.NoError(t, err)
	require.NotNil(t, c.SecurityContext)
	assert.True(t, *c.SecurityContext.ReadOnlyRootFilesystem, "rootfs stays read-only")

	var tmpMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == "/tmp" {
			tmpMount = &c.VolumeMounts[i]
		}
	}
	require.NotNil(t, tmpMount, "a /tmp mount must exist for read-only-rootfs scratch")
	assert.False(t, tmpMount.ReadOnly, "/tmp must be writable")
	assert.Equal(t, "tmp-det", tmpMount.Name)

	var tmpVol *corev1.Volume
	for i := range vols {
		if vols[i].Name == tmpMount.Name {
			tmpVol = &vols[i]
		}
	}
	require.NotNil(t, tmpVol, "the /tmp volume must be declared")
	require.NotNil(t, tmpVol.EmptyDir, "/tmp must be an emptyDir")
}

func TestBuildContainer_noSource_returnsError(t *testing.T) {
	_, _, err := BuildContainer(Spec{Ref: "bad", Port: 8080})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"bad"`)
}

func TestBuildContainer_envFrom(t *testing.T) {
	c, _, err := BuildContainer(Spec{
		Ref:     "creds",
		Image:   "example.com/img:latest",
		Port:    8080,
		EnvFrom: "my-session-secret",
	})
	require.NoError(t, err)
	require.Len(t, c.EnvFrom, 1)
	assert.Equal(t, "my-session-secret", c.EnvFrom[0].SecretRef.Name)
}

func TestBuildContainer_mcpPortEnv(t *testing.T) {
	c, _, err := BuildContainer(Spec{
		Ref:   "srv",
		Image: "example.com/srv:latest",
		Port:  18080,
	})
	require.NoError(t, err)
	var found bool
	for _, e := range c.Env {
		if e.Name == "MCP_PORT" {
			assert.Equal(t, "18080", e.Value)
			found = true
		}
	}
	assert.True(t, found, "MCP_PORT env var must be set")
}

func TestBuildContainer_configEnv(t *testing.T) {
	c, _, err := BuildContainer(Spec{
		Ref:    "srv",
		Image:  "example.com/srv:latest",
		Port:   18080,
		Config: "x",
	})
	require.NoError(t, err)
	var mcpPort, sidecarConfig bool
	for _, e := range c.Env {
		switch e.Name {
		case "MCP_PORT":
			mcpPort = true
		case "AP_SIDECAR_CONFIG":
			sidecarConfig = true
			assert.Equal(t, "x", e.Value)
		}
	}
	assert.True(t, mcpPort, "MCP_PORT env var must still be set")
	assert.True(t, sidecarConfig, "AP_SIDECAR_CONFIG env var must be set from Spec.Config")
}

func TestBuildContainer_emptyConfig_noEnvVar(t *testing.T) {
	c, _, err := BuildContainer(Spec{
		Ref:   "srv",
		Image: "example.com/srv:latest",
		Port:  18080,
	})
	require.NoError(t, err)
	for _, e := range c.Env {
		assert.NotEqual(t, "AP_SIDECAR_CONFIG", e.Name, "empty Config must not set the env var at all")
	}
}

func TestBuildNetworkPolicy_allowlistEgress_hasDNSAndHTTPS(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "ns"}}
	np := BuildNetworkPolicy(sess, Spec{
		Ref:  "tool",
		Port: 18080,
		Egress: EgressPolicy{
			AllowlistMode: spiceboxv1alpha1.NetworkModeAllowlist,
		},
	})
	require.NotNil(t, np)
	// allowlist: DNS rule + external 443/80
	assert.NotEmpty(t, np.Spec.Egress, "allowlist egress must have rules")
	assert.Len(t, np.Spec.Egress, 2, "DNS rule + external HTTPS rule")
}

func TestBuildNetworkPolicy_labels(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: "ns"}}
	np := BuildNetworkPolicy(sess, Spec{Ref: "myref", Port: 8080, Egress: EgressPolicy{Denied: true}})
	require.NotNil(t, np)
	assert.Equal(t, "s3", np.Labels[labelSession])
	assert.Equal(t, "myref", np.Labels[labelSidecar])
	assert.Equal(t, "sidecar-s3-myref", np.Name)
	assert.Equal(t, "ns", np.Namespace)
}

func TestBuildNetworkPolicy_podSelector(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s4", Namespace: "ns"}}
	np := BuildNetworkPolicy(sess, Spec{Ref: "det", Port: 8080, Egress: EgressPolicy{Denied: true}})
	require.NotNil(t, np)
	assert.Equal(t, "s4", np.Spec.PodSelector.MatchLabels[labelSession])
	assert.Equal(t, "det", np.Spec.PodSelector.MatchLabels[labelSidecar])
}

func TestBuildNetworkPolicy_ownerRef(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s5", Namespace: "ns", UID: "test-uid"},
	}
	np := BuildNetworkPolicy(sess, Spec{Ref: "det", Port: 8080, Egress: EgressPolicy{Denied: true}})
	require.Len(t, np.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", np.OwnerReferences[0].Kind)
	assert.Equal(t, "s5", np.OwnerReferences[0].Name)
	assert.Equal(t, "test-uid", string(np.OwnerReferences[0].UID))
	assert.True(t, *np.OwnerReferences[0].Controller)
}

func TestBuildPod_labelsAndSAToken(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess1", Namespace: "ns", UID: "uid1"},
	}
	pod, vols, err := BuildPod(sess, Spec{
		Ref:   "det",
		Image: "example.com/det@sha256:abc",
		Port:  8919,
	})
	require.NoError(t, err)
	// The writable /tmp emptyDir must be present on the pod (read-only rootfs
	// scratch), as must the SA-token shadow — inert here, since this pod sets
	// automount=false, but the container builder applies it unconditionally.
	require.NotNil(t, volumeNamed(t, vols, "tmp-det").EmptyDir)
	require.NotNil(t, volumeNamed(t, vols, "no-sa-token-det").EmptyDir)
	assert.Equal(t, vols, pod.Spec.Volumes, "BuildPod must wire the container volumes into the pod")
	assert.Equal(t, "sidecar-sess1-det", pod.Name)
	assert.Equal(t, "ns", pod.Namespace)
	// label: session and sidecar ref
	assert.Equal(t, "sess1", pod.Labels[labelSession])
	assert.Equal(t, "det", pod.Labels[labelSidecar])
	// SA token must be disabled
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	// owner ref
	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "AgentSession", pod.OwnerReferences[0].Kind)
}

func TestBuildPod_propagatesContainerError(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
	_, _, err := BuildPod(sess, Spec{Ref: "bad", Port: 8080}) // no image or inline
	require.Error(t, err)
}

func TestBuildContainer_startupProbeDefaults(t *testing.T) {
	c, _, err := BuildContainer(Spec{
		Ref:   "srv",
		Image: "example.com/srv:latest",
		Port:  9000,
		// Health left zero: should default to /healthz + 30s
	})
	require.NoError(t, err)
	require.NotNil(t, c.StartupProbe)
	assert.Equal(t, "/healthz", c.StartupProbe.HTTPGet.Path)
	assert.Equal(t, int32(30), c.StartupProbe.FailureThreshold)
}

func TestBuildNetworkPolicy_customName(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s7", Namespace: "ns"}}
	cases := []struct {
		name     string
		specName string
		wantName string
	}{
		{
			name:     "non-empty Spec.Name overrides default",
			specName: "s7-sidecar-myref-netpol",
			wantName: "s7-sidecar-myref-netpol",
		},
		{
			name:     "empty Spec.Name falls back to default",
			specName: "",
			wantName: "sidecar-s7-myref",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			np := BuildNetworkPolicy(sess, Spec{
				Ref:    "myref",
				Name:   tc.specName,
				Port:   8080,
				Egress: EgressPolicy{Denied: true},
			})
			require.NotNil(t, np)
			assert.Equal(t, tc.wantName, np.Name)
		})
	}
}

func TestBuildPod_imagePullSecret(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns", UID: "u"}}
	// set → referenced
	pod, _, err := BuildPod(sess, Spec{Ref: "det", Image: "x:dev", Port: 8919, ImagePullSecret: "myreg-creds"})
	require.NoError(t, err)
	require.Len(t, pod.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "myreg-creds", pod.Spec.ImagePullSecrets[0].Name)
	// unset → nil
	pod2, _, _ := BuildPod(sess, Spec{Ref: "det", Image: "x:dev", Port: 8919})
	assert.Empty(t, pod2.Spec.ImagePullSecrets)
}

func TestBuildNetworkPolicy_ingressRunnerOnly(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s6", Namespace: "ns"}}
	np := BuildNetworkPolicy(sess, Spec{Ref: "det", Port: 7777, Egress: EgressPolicy{Denied: true}})
	require.Len(t, np.Spec.Ingress, 1)
	// The ingress rule has a peer that includes the session label + DoesNotExist on sidecar label
	require.Len(t, np.Spec.Ingress[0].From, 1)
	peer := np.Spec.Ingress[0].From[0]
	require.NotNil(t, peer.PodSelector)
	assert.Equal(t, "s6", peer.PodSelector.MatchLabels[labelSession])
	require.Len(t, peer.PodSelector.MatchExpressions, 1)
	assert.Equal(t, metav1.LabelSelectorOpDoesNotExist, peer.PodSelector.MatchExpressions[0].Operator)
	assert.Equal(t, labelSidecar, peer.PodSelector.MatchExpressions[0].Key)
}
