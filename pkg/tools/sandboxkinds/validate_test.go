package sandboxkinds_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
)

// featureKind reports a fixed capability set, standing in for a backend that
// cannot do what a class asks.
type featureKind struct{ supports map[sandboxkinds.Feature]bool }

func (featureKind) Name() string                                   { return "feature-stub" }
func (f featureKind) Supports(x sandboxkinds.Feature) bool         { return f.supports[x] }
func (featureKind) WorkspaceDomain() string                        { return "stub" }
func (featureKind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }
func (featureKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, nil
}

// noUnpackKind stands in for a backend that can mount a ConfigMap directly
// but cannot run an initContainer to expand one first — the case a tarGz
// mount must be refused for.
type noUnpackKind struct{}

func (noUnpackKind) Name() string { return "no-unpack-stub" }
func (noUnpackKind) Supports(f sandboxkinds.Feature) bool {
	return f == sandboxkinds.FeatureConfigMapMounts
}
func (noUnpackKind) WorkspaceDomain() string                        { return "stub" }
func (noUnpackKind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }
func (noUnpackKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, nil
}

// rawOnlyKind has the identical capability set to noUnpackKind: it exists as
// a separate fake so the raw-mount test below reads as its own scenario
// (a backend that predates FeatureUnpackMounts must still accept the mounts
// it always could) rather than reusing a fake named for the rejection case.
type rawOnlyKind struct{}

func (rawOnlyKind) Name() string { return "raw-only-stub" }
func (rawOnlyKind) Supports(f sandboxkinds.Feature) bool {
	return f == sandboxkinds.FeatureConfigMapMounts
}
func (rawOnlyKind) WorkspaceDomain() string                        { return "stub" }
func (rawOnlyKind) ValidateClass(v1alpha1.SpiceboxClassSpec) error { return nil }
func (rawOnlyKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, nil
}

func TestValidateClassAgainstKind_RejectsUnsupportedFeature(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*v1alpha1.SpiceboxClassSpec)
		supports map[sandboxkinds.Feature]bool
		wantErr  string
	}{
		{
			name:     "class mounts a ConfigMap the backend cannot mount: rejected",
			mutate:   func(s *v1alpha1.SpiceboxClassSpec) { s.Mounts = []v1alpha1.SpiceboxMount{{Name: "cfg"}} },
			supports: map[sandboxkinds.Feature]bool{},
			wantErr:  "configmap-mounts",
		},
		{
			name:     "class names a toolchain the backend cannot overlay: rejected",
			mutate:   func(s *v1alpha1.SpiceboxClassSpec) { s.Toolchains = []string{"go"} },
			supports: map[sandboxkinds.Feature]bool{},
			wantErr:  "toolchain-overlay",
		},
		{
			name:     "backend supports what the class asks: accepted",
			mutate:   func(s *v1alpha1.SpiceboxClassSpec) { s.Toolchains = []string{"go"} },
			supports: map[sandboxkinds.Feature]bool{sandboxkinds.FeatureToolchainOverlay: true},
		},
		{
			name:     "class asks for nothing optional: accepted by any backend",
			mutate:   func(*v1alpha1.SpiceboxClassSpec) {},
			supports: map[sandboxkinds.Feature]bool{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := v1alpha1.SpiceboxClassSpec{}
			tc.mutate(&spec)

			err := sandboxkinds.ValidateClassAgainstKind(featureKind{supports: tc.supports}, spec)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr, "the error must name the unsupported feature")
			assert.Contains(t, err.Error(), "feature-stub", "the error must name the backend")
		})
	}
}

func TestValidate_TarGzMountRequiresUnpackFeature(t *testing.T) {
	spec := v1alpha1.SpiceboxClassSpec{
		Mounts: []v1alpha1.SpiceboxMount{{
			Name: "demo-skill", MountPath: "/skills/demo",
			Format: v1alpha1.MountFormatTarGz,
			Source: v1alpha1.MountSource{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
			},
		}},
	}

	err := sandboxkinds.ValidateClassAgainstKind(noUnpackKind{}, spec)
	require.Error(t, err, "a kind that cannot run initContainers must refuse a tarGz mount")
	assert.Contains(t, err.Error(), "spec.mounts", "the error must name the field")
	assert.Contains(t, err.Error(), string(sandboxkinds.FeatureUnpackMounts))
}

func TestValidate_RawMountDoesNotRequireUnpackFeature(t *testing.T) {
	spec := v1alpha1.SpiceboxClassSpec{
		Mounts: []v1alpha1.SpiceboxMount{{
			// Format is left unset (the empty string), NOT "raw" — an in-memory
			// struct never sees the CRD default; only the API server applies it.
			// A predicate that mishandled the empty case would wrongly demand
			// FeatureUnpackMounts here.
			Name: "demo-cfg", MountPath: "/etc/demo",
			Source: v1alpha1.MountSource{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
			},
		}},
	}
	require.NoError(t, sandboxkinds.ValidateClassAgainstKind(rawOnlyKind{}, spec),
		"raw mounts predate this feature and must not start requiring it")
}

// TestValidateSessionAgainstKind_RejectsUnsupportedFeature pins the fix for
// the gap where a staged skill's tarGz mount lands on
// SpiceboxSessionSpec.Mounts (session provenance) rather than on the class's
// own spec.mounts, and ValidateClassAgainstKind — which only ever looks at
// the class — never saw it. Both shipped kinds happen to support every
// feature today, so this gap was invisible in production; it only bites a
// future kind that declares FeatureUnpackMounts (or FeatureConfigMapMounts)
// unsupported.
func TestValidateSessionAgainstKind_RejectsUnsupportedFeature(t *testing.T) {
	tarGzMount := []v1alpha1.SpiceboxMount{{
		Name: "demo-skill", MountPath: "/skills/demo",
		Format: v1alpha1.MountFormatTarGz,
		Source: v1alpha1.MountSource{
			ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
		},
	}}
	rawMount := []v1alpha1.SpiceboxMount{{
		Name: "demo-cfg", MountPath: "/etc/demo",
		Source: v1alpha1.MountSource{
			ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
		},
	}}

	cases := []struct {
		name               string
		kind               sandboxkinds.Kind
		sessionMounts      []v1alpha1.SpiceboxMount
		legacySkillBundles bool
		wantErr            string
	}{
		{
			name:          "session tarGz mount against a kind lacking unpack: rejected",
			kind:          noUnpackKind{},
			sessionMounts: tarGzMount,
			wantErr:       string(sandboxkinds.FeatureUnpackMounts),
		},
		{
			name:          "session raw mount against a kind lacking configmap-mounts: rejected",
			kind:          featureKind{supports: map[sandboxkinds.Feature]bool{}},
			sessionMounts: rawMount,
			wantErr:       string(sandboxkinds.FeatureConfigMapMounts),
		},
		{
			name:          "session raw mount against a kind that only supports configmap-mounts: accepted",
			kind:          rawOnlyKind{},
			sessionMounts: rawMount,
		},
		{
			name:               "legacy SkillBundles fallback alone (no session.Mounts) still demands unpack",
			kind:               noUnpackKind{},
			legacySkillBundles: true,
			wantErr:            string(sandboxkinds.FeatureUnpackMounts),
		},
		{
			name: "no session mounts, no legacy fallback: accepted by any kind",
			kind: featureKind{supports: map[sandboxkinds.Feature]bool{}},
		},
		{
			name:          "session tarGz mount accepted by the real pod kind",
			kind:          pod.Kind{},
			sessionMounts: tarGzMount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sandboxkinds.ValidateSessionAgainstKind(tc.kind, tc.sessionMounts, tc.legacySkillBundles)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr, "the error must name the unsupported feature")
		})
	}
}

// TestValidate_TarGzMountAcceptedByPodKind is
// TestValidate_TarGzMountRequiresUnpackFeature's sibling run against the REAL
// pod kind instead of a fake. It must succeed because the pod kind declares
// FeatureUnpackMounts — and it is the case the negative control in Step 6
// turns red when that declaration is removed.
func TestValidate_TarGzMountAcceptedByPodKind(t *testing.T) {
	spec := v1alpha1.SpiceboxClassSpec{
		Mounts: []v1alpha1.SpiceboxMount{{
			Name: "demo-skill", MountPath: "/skills/demo",
			Format: v1alpha1.MountFormatTarGz,
			Source: v1alpha1.MountSource{
				ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
			},
		}},
	}
	assert.NoError(t, sandboxkinds.ValidateClassAgainstKind(pod.Kind{}, spec),
		"the pod kind declares FeatureUnpackMounts, so a tarGz mount must be accepted")
}
