package image_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/image"
	"github.com/authzed/openagentprimitives/pkg/tools/toolchain/kinds/registry"
)

func mount() spiceboxv1alpha1.ToolchainMount {
	return spiceboxv1alpha1.ToolchainMount{
		Name:       "go",
		SourceKind: "image",
		Image:      "ghcr.io/example/ap-toolchain-go@sha256:aaaa",
		Prefix:     "/opt/ap-toolchains/go",
		Bin:        []string{"bin"},
		SizeBytes:  100,
	}
}

func TestImageKind_RegisteredUnderImage(t *testing.T) {
	k, ok := registry.ByKind("image")
	require.True(t, ok, "the image kind must self-register via init()")
	assert.Equal(t, "image", k.Name())
}

func TestImageKind_Validate(t *testing.T) {
	k := image.Kind{}
	assert.NoError(t, k.Validate(mount()))

	m := mount()
	m.Image = ""
	assert.ErrorContains(t, k.Validate(m), "image is required")

	m = mount()
	m.Prefix = "opt/ap-toolchains/go"
	assert.ErrorContains(t, k.Validate(m), "must be absolute")

	m = mount()
	m.Name = "go.1.21"
	assert.ErrorContains(t, k.Validate(m), "DNS-1123 label",
		"m.Name becomes \"toolchain-\"+m.Name, a corev1.Container.Name, which must be a DNS-1123 label")
}

func TestImageKind_Apply_AddsCopyInitContainer(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox"}}}}
	sc := &corev1.SecurityContext{}
	require.NoError(t, image.Kind{}.Apply(toolchain.ApplyParams{
		Pod: pod, Mount: mount(), VolumeName: "toolchains",
		InitMountPath: "/dst", SecurityContext: sc,
	}), "Apply")

	require.Len(t, pod.Spec.InitContainers, 1, "one init container per toolchain")
	ic := pod.Spec.InitContainers[0]
	assert.Equal(t, "toolchain-go", ic.Name)
	assert.Equal(t, "ghcr.io/example/ap-toolchain-go@sha256:aaaa", ic.Image,
		"the init container runs the toolchain's own image — that is where the payload lives")
	assert.Same(t, sc, ic.SecurityContext, "init container inherits the hardened context")

	require.Len(t, ic.Command, 3)
	assert.Equal(t, []string{"/bin/sh", "-c"}, ic.Command[:2])
	script := ic.Command[2]
	assert.Contains(t, script, `mkdir -p '/dst/go'`,
		"single-quoted, not %q-quoted: %q leaves $ and backtick live for the shell")
	assert.Contains(t, script, `cp -R '/opt/ap-toolchains/go/.' '/dst/go'`,
		"cp -R, never cp -a: uid 1000 with drop:[ALL] cannot chown")
	assert.NotContains(t, script, "cp -a", "cp -a would attempt chown and fail")

	require.Len(t, ic.VolumeMounts, 1)
	assert.Equal(t, "toolchains", ic.VolumeMounts[0].Name)
	assert.Equal(t, "/dst", ic.VolumeMounts[0].MountPath)
	assert.False(t, ic.VolumeMounts[0].ReadOnly, "the init container writes the shared volume")
}

func TestImageKind_Apply_InvalidMountLeavesPodUnmutated(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox"}}}}
	m := mount()
	m.Name = "go.1.21" // not a DNS-1123 label

	err := image.Kind{}.Apply(toolchain.ApplyParams{
		Pod: pod, Mount: m, VolumeName: "toolchains",
		InitMountPath: "/dst", SecurityContext: &corev1.SecurityContext{},
	})

	require.Error(t, err, "Apply must reject an invalid Mount.Name")
	assert.ErrorContains(t, err, "DNS-1123 label")
	assert.Empty(t, pod.Spec.InitContainers,
		"a rejected Apply must not half-mutate the pod with a partially built init container")
}
