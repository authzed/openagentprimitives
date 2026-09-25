package podspec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// cmSource builds a MountSource pointing at a ConfigMap by name — the only
// recognised source today.
func cmSource(name string) spiceboxv1alpha1.MountSource {
	return spiceboxv1alpha1.MountSource{
		ConfigMapRef: &corev1.LocalObjectReference{Name: name},
	}
}

func TestApplyMounts_TarGzExpandsUnderOneSharedVolume(t *testing.T) {
	var spec corev1.PodSpec
	spec.Containers = []corev1.Container{{Name: "sandbox"}}

	require.NoError(t, applyMounts(&spec, []spiceboxv1alpha1.SpiceboxMount{
		{Name: "code-review", MountPath: "/skills/code-review",
			Format: spiceboxv1alpha1.MountFormatTarGz,
			Source: cmSource("demo-cm-a")},
		{Name: "style-guide", MountPath: "/skills/style-guide",
			Format: spiceboxv1alpha1.MountFormatTarGz,
			Source: cmSource("demo-cm-b")},
	}, "demo-init:latest"))

	assert.Len(t, spec.InitContainers, 1,
		"N tarGz mounts share ONE initContainer, not one each")

	// The claim that matters: each mount lands at its own path, isolated by subPath.
	got := map[string]corev1.VolumeMount{}
	for _, vm := range spec.Containers[0].VolumeMounts {
		got[vm.MountPath] = vm
	}
	require.Contains(t, got, "/skills/code-review")
	require.Contains(t, got, "/skills/style-guide")
	assert.Equal(t, "code-review", got["/skills/code-review"].SubPath)
	assert.Equal(t, "style-guide", got["/skills/style-guide"].SubPath)
	assert.Equal(t, got["/skills/code-review"].Name, got["/skills/style-guide"].Name,
		"both subPaths come from the same shared unpack volume")
	assert.True(t, got["/skills/code-review"].ReadOnly, "staged content is never writable")
}

func TestApplyMounts_RawEmitsNoInitContainer(t *testing.T) {
	var spec corev1.PodSpec
	spec.Containers = []corev1.Container{{Name: "sandbox"}}

	require.NoError(t, applyMounts(&spec, []spiceboxv1alpha1.SpiceboxMount{
		{Name: "demo-cfg", MountPath: "/etc/demo", Source: cmSource("demo-cm")},
	}, "demo-init:latest"))

	assert.Empty(t, spec.InitContainers,
		"a raw-only pod must be byte-identical to its pre-format shape")

	// The name alone only pinned the initContainer's absence. Pin the rest of
	// the claim: no unpack volume snuck in alongside the ConfigMap volume, and
	// the VolumeMount is the exact shape the pre-Format code produced — in
	// particular ReadOnly stays true and SubPath stays unset, or a raw mount
	// can silently become writable (or subPath-scoped into a shared volume it
	// was never given) without any test noticing.
	require.Len(t, spec.Volumes, 1, "a raw-only pod must gain no unpack volume")
	assert.NotNil(t, spec.Volumes[0].ConfigMap, "the one volume must be the ConfigMap mount, not an emptyDir")

	require.Len(t, spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, corev1.VolumeMount{
		Name:      "demo-cfg",
		MountPath: "/etc/demo",
		ReadOnly:  true,
	}, spec.Containers[0].VolumeMounts[0],
		"a raw mount's VolumeMount must be byte-identical to its pre-Format shape: read-only, no SubPath")
}

func TestApplyMounts_DigestIsVerifiedBeforeExtraction(t *testing.T) {
	var spec corev1.PodSpec
	spec.Containers = []corev1.Container{{Name: "sandbox"}}

	require.NoError(t, applyMounts(&spec, []spiceboxv1alpha1.SpiceboxMount{
		{Name: "code-review", MountPath: "/skills/code-review",
			Format: spiceboxv1alpha1.MountFormatTarGz,
			Digest: "sha256:0123456789abcdef", Source: cmSource("demo-cm")},
	}, "demo-init:latest"))

	script := strings.Join(spec.InitContainers[0].Command, " ") +
		" " + strings.Join(spec.InitContainers[0].Args, " ")
	assert.Contains(t, script, "0123456789abcdef",
		"the expected digest must reach the initContainer, or nothing verifies it")
	assert.Contains(t, script, "sha256sum",
		"verification happens before extraction, in the container that extracts")
}

// TestApplyMounts_NoDigestSkipsVerification pins the other half of §3.4: an
// empty Digest must not gain a check, so a raw or un-digested tarGz mount's
// init container stays byte-identical whether or not this feature exists.
func TestApplyMounts_NoDigestSkipsVerification(t *testing.T) {
	var spec corev1.PodSpec
	spec.Containers = []corev1.Container{{Name: "sandbox"}}

	require.NoError(t, applyMounts(&spec, []spiceboxv1alpha1.SpiceboxMount{
		{Name: "code-review", MountPath: "/skills/code-review",
			Format: spiceboxv1alpha1.MountFormatTarGz,
			Source: cmSource("demo-cm")},
	}, "demo-init:latest"))

	script := strings.Join(spec.InitContainers[0].Command, " ") +
		" " + strings.Join(spec.InitContainers[0].Args, " ")
	assert.NotContains(t, script, "sha256sum",
		"an empty Digest must not gain a check — un-digested mounts stay unchanged")
}

// TestApplyMounts_UnknownSourceFailsClosed pins that applyMounts, not just
// BuildClassSpec, is where the fail-closed check on an unrecognised
// MountSource lives now that BuildClassSpec delegates to it.
func TestApplyMounts_UnknownSourceFailsClosed(t *testing.T) {
	var spec corev1.PodSpec
	spec.Containers = []corev1.Container{{Name: "sandbox"}}

	err := applyMounts(&spec, []spiceboxv1alpha1.SpiceboxMount{
		{Name: "demo", MountPath: "/demo"}, // Source has no member set
	}, "demo-init:latest")

	require.Error(t, err, "an unrecognised mount source must fail closed, not mount nothing")
	assert.Contains(t, err.Error(), "demo", "the error must name the offending mount")
}
