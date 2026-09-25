package podspec_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

func demoClass() spiceboxv1alpha1.SpiceboxClassSpec {
	return spiceboxv1alpha1.SpiceboxClassSpec{
		Image: "demo.invalid/spicebox-sandbox:test",
		Resources: spiceboxv1alpha1.SpiceboxResources{
			CPU:              resource.MustParse("1"),
			Memory:           resource.MustParse("256Mi"),
			EphemeralStorage: resource.MustParse("1Gi"),
		},
	}
}

func demoSession(name string) *spiceboxv1alpha1.SpiceboxSession {
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(name + "-uid"),
		},
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{Class: "demo-class"},
	}
}

// The eligibility rule for pre-warming: a session may adopt a warm sandbox
// only when its PodSpec is entirely class-derived. This is asserted as an
// EQUALITY rather than a feature check, so that a future session-specific
// feature makes such sessions ineligible automatically instead of silently
// handing them a warm sandbox missing the thing.
func TestBuildClassSpec_MatchesAPlainSessionsPodSpec(t *testing.T) {
	class := demoClass()

	classSpec, err := podspec.BuildClassSpec(class, nil)
	require.NoError(t, err)

	pod, err := podspec.Build(demoSession("demo-session"), class)
	require.NoError(t, err)

	require.Empty(t, cmp.Diff(classSpec, pod.Spec),
		"a session with no workspace, no skill bundles and no resolved toolchains "+
			"must render exactly the class-derived PodSpec")
}

// One case per session-derived input. Each must make the session's PodSpec
// DIFFER from the class spec — that difference is what makes it ineligible.
func TestBuildClassSpec_DiffersForEachSessionSpecificInput(t *testing.T) {
	class := demoClass()
	classSpec, err := podspec.BuildClassSpec(class, nil)
	require.NoError(t, err)

	cases := []struct {
		name   string
		mutate func(*spiceboxv1alpha1.SpiceboxSession)
	}{
		{
			name: "shared workspace adds a PVC volume",
			mutate: func(s *spiceboxv1alpha1.SpiceboxSession) {
				s.Spec.Workspace = spiceboxv1alpha1.WorkspaceConfig{
					Mode: spiceboxv1alpha1.WorkspaceShared, SharedClaimName: "demo-workspace",
				}
			},
		},
		{
			name: "skill bundles add an init container",
			mutate: func(s *spiceboxv1alpha1.SpiceboxSession) {
				s.Spec.SkillBundles = []spiceboxv1alpha1.SkillBundleMount{
					{MountName: "demo", ConfigMapName: "demo-skill"},
				}
			},
		},
		{
			name: "resolved toolchains overlay the pod",
			mutate: func(s *spiceboxv1alpha1.SpiceboxSession) {
				s.Status.ResolvedToolchains = []spiceboxv1alpha1.ToolchainMount{
					{
						Name:       "demo-toolchain",
						SourceKind: "image",
						Image:      "demo.invalid/toolchain:test",
						Prefix:     "/toolchains/demo",
					},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+": session PodSpec differs from the class spec", func(t *testing.T) {
			s := demoSession("demo-session")
			tc.mutate(s)

			pod, err := podspec.Build(s, class)
			require.NoError(t, err)
			require.NotEmpty(t, cmp.Diff(classSpec, pod.Spec),
				"this input must make the session ineligible for a warm sandbox")
		})
	}
}

// A session whose ONLY difference from the class is its frozen toolchains must
// render exactly BuildClassSpec(class, mounts). This is what makes a toolchain
// class poolable: the warm template can be built from the same inputs.
func TestBuildClassSpec_WithToolchains_MatchesAToolchainSessionsPodSpec(t *testing.T) {
	class := demoClass()
	class.Toolchains = []string{"demo-toolchain"}

	mounts := []spiceboxv1alpha1.ToolchainMount{{
		Name:       "demo-toolchain",
		SourceKind: "image",
		Image:      "demo.invalid/toolchain:test",
		Prefix:     "/toolchains/demo",
		Bin:        []string{"bin"},
		SizeBytes:  1024,
	}}

	classSpec, err := podspec.BuildClassSpec(class, mounts)
	require.NoError(t, err)

	// Name its own behaviour, not just the equality: an equality claim alone
	// would still pass if applyToolchainsToSpec's body were deleted (both
	// sides would be equally empty of a toolchains volume).
	volNames := make([]string, 0, len(classSpec.Volumes))
	for _, v := range classSpec.Volumes {
		volNames = append(volNames, v.Name)
	}
	require.Contains(t, volNames, "toolchains",
		"BuildClassSpec with a non-empty mounts slice must actually render the toolchain overlay")

	s := demoSession("demo-session")
	s.Status.ResolvedToolchains = mounts

	pod, err := podspec.Build(s, class)
	require.NoError(t, err)

	require.Empty(t, cmp.Diff(classSpec, pod.Spec),
		"a session whose only session-scoped input is its frozen toolchains "+
			"must render exactly the class-derived PodSpec for those toolchains")
}

// Different toolchain sets must render different PodSpecs, or the template hash
// could not distinguish them and a session would adopt a pod missing a compiler.
func TestBuildClassSpec_DifferentToolchainSetsRenderDifferently(t *testing.T) {
	class := demoClass()
	class.Toolchains = []string{"demo-toolchain"}

	one := []spiceboxv1alpha1.ToolchainMount{{
		Name: "demo-toolchain", SourceKind: "image",
		Image: "demo.invalid/toolchain:test", Prefix: "/toolchains/demo",
		Bin: []string{"bin"}, SizeBytes: 1024,
	}}
	two := append(append([]spiceboxv1alpha1.ToolchainMount{}, one...),
		spiceboxv1alpha1.ToolchainMount{
			Name: "other-toolchain", SourceKind: "image",
			Image: "demo.invalid/other:test", Prefix: "/toolchains/other",
			Bin: []string{"bin"}, SizeBytes: 2048,
		})

	a, err := podspec.BuildClassSpec(class, one)
	require.NoError(t, err)
	b, err := podspec.BuildClassSpec(class, two)
	require.NoError(t, err)

	require.NotEmpty(t, cmp.Diff(a, b),
		"a second toolchain must change the rendered spec")
}

// The invariant that makes a single equality check ("session PodSpec ==
// BuildClassSpec(class, mounts)") a sufficient eligibility test at all:
// class-derived content is a CONTIGUOUS PREFIX of Volumes,
// Containers[0].VolumeMounts, and InitContainers, and every session-derived
// input (workspace, skill bundles, toolchains — the latter now applied
// inside BuildClassSpec itself) only ever APPENDS. Nothing may splice into
// the middle of what BuildClassSpec already produced, and nothing may mutate
// an existing element in place. A session carrying every session-derived
// input at once is the case that would expose a violation, since a single
// mutate-in-place or insert-in-the-middle wouldn't necessarily show up as a
// non-equal whole-slice diff if it happened to net out to the same elements
// in a different order for a smaller combination.
func TestBuildClassSpec_IsAContiguousPrefixWithEverySessionInputCombined(t *testing.T) {
	class := demoClass()
	class.Toolchains = []string{"demo-toolchain"}

	mounts := []spiceboxv1alpha1.ToolchainMount{{
		Name:       "demo-toolchain",
		SourceKind: "image",
		Image:      "demo.invalid/toolchain:test",
		Prefix:     "/toolchains/demo",
		Bin:        []string{"bin"},
		SizeBytes:  1024,
	}}

	classSpec, err := podspec.BuildClassSpec(class, mounts)
	require.NoError(t, err)

	s := demoSession("demo-session")
	s.Status.ResolvedToolchains = mounts
	s.Spec.Workspace = spiceboxv1alpha1.WorkspaceConfig{
		Mode: spiceboxv1alpha1.WorkspaceShared, SharedClaimName: "demo-workspace",
	}
	s.Spec.SkillBundles = []spiceboxv1alpha1.SkillBundleMount{
		{MountName: "demo-skill", ConfigMapName: "demo-skill-configmap"},
	}

	pod, err := podspec.Build(s, class)
	require.NoError(t, err)

	// Preconditions: the session's slices must be at least as long as the
	// class's, or slicing to len(classSpec.X) below would panic. A session
	// with every input added should only ever grow these slices.
	require.GreaterOrEqual(t, len(pod.Spec.Volumes), len(classSpec.Volumes))
	require.GreaterOrEqual(t, len(pod.Spec.Containers[0].VolumeMounts), len(classSpec.Containers[0].VolumeMounts))
	require.GreaterOrEqual(t, len(pod.Spec.InitContainers), len(classSpec.InitContainers))

	assert.Empty(t, cmp.Diff(classSpec.Volumes, pod.Spec.Volumes[:len(classSpec.Volumes)]),
		"class-derived content is a contiguous prefix; session-derived inputs only append")
	assert.Empty(t, cmp.Diff(
		classSpec.Containers[0].VolumeMounts,
		pod.Spec.Containers[0].VolumeMounts[:len(classSpec.Containers[0].VolumeMounts)]),
		"class-derived content is a contiguous prefix; session-derived inputs only append")
	assert.Empty(t, cmp.Diff(classSpec.InitContainers, pod.Spec.InitContainers[:len(classSpec.InitContainers)]),
		"class-derived content is a contiguous prefix; session-derived inputs only append")
}
