package spiceboxsession

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
	sandboxregistry "github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/registry"
)

func TestApplySandboxOverride(t *testing.T) {
	cases := []struct {
		name     string
		class    spiceboxv1alpha1.SandboxBackend
		override *spiceboxv1alpha1.SandboxBackend
		wantKind string
	}{
		{
			name:     "no override: the class's own backend is frozen unchanged",
			class:    spiceboxv1alpha1.SandboxBackend{Kind: "class-kind"},
			override: nil,
			wantKind: "class-kind",
		},
		{
			name:     "override present: it wins over the class",
			class:    spiceboxv1alpha1.SandboxBackend{Kind: "class-kind"},
			override: &spiceboxv1alpha1.SandboxBackend{Kind: "override-kind"},
			wantKind: "override-kind",
		},
		{
			name:     "override onto a class that declared nothing",
			class:    spiceboxv1alpha1.SandboxBackend{},
			override: &spiceboxv1alpha1.SandboxBackend{Kind: "override-kind"},
			wantKind: "override-kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applySandboxOverride(
				spiceboxv1alpha1.SpiceboxClassSpec{Image: "demo.invalid/img", Sandbox: tc.class},
				tc.override)

			assert.Equal(t, tc.wantKind, got.Sandbox.Kind)
			assert.Equal(t, "demo.invalid/img", got.Image,
				"applying the override must not disturb the rest of the snapshot")
		})
	}
}

// The override replaces the class's backend wholesale rather than merging into
// it. Cross-tier config merging already happened in the settings resolver; a
// second, differently-shaped merge here would be a competing source of truth.
func TestApplySandboxOverride_ReplacesConfigWholesale(t *testing.T) {
	class := spiceboxv1alpha1.SandboxBackend{
		Kind:   "same-kind",
		Config: &apiextensionsv1.JSON{Raw: []byte(`{"from":"class"}`)},
	}
	override := &spiceboxv1alpha1.SandboxBackend{
		Kind:   "same-kind",
		Config: &apiextensionsv1.JSON{Raw: []byte(`{"from":"override"}`)},
	}

	got := applySandboxOverride(spiceboxv1alpha1.SpiceboxClassSpec{Sandbox: class}, override)

	assert.JSONEq(t, `{"from":"override"}`, string(got.Sandbox.Config.Raw))
}

// The caller's class snapshot must not be mutated — it is shared with the
// object being reconciled.
func TestApplySandboxOverride_DoesNotMutateInput(t *testing.T) {
	in := spiceboxv1alpha1.SpiceboxClassSpec{
		Sandbox: spiceboxv1alpha1.SandboxBackend{Kind: "class-kind"},
	}
	_ = applySandboxOverride(in, &spiceboxv1alpha1.SandboxBackend{Kind: "override-kind"})

	assert.Equal(t, "class-kind", in.Sandbox.Kind, "input must be untouched")
}

// rejectingKind proves the BACKEND'S OWN veto is reached. Registered under a
// name nothing else looks up, and never unregistered: the registry has no
// per-kind removal, and Reset() would wipe the init()-time "pod" registration
// the rest of this package's tests depend on (nothing re-runs init).
type rejectingKind struct{}

func (rejectingKind) Name() string                       { return "test-rejecting" }
func (rejectingKind) Supports(sandboxkinds.Feature) bool { return true }
func (rejectingKind) WorkspaceDomain() string            { return sandboxkinds.DomainKubernetesPVC }
func (rejectingKind) ValidateClass(spiceboxv1alpha1.SpiceboxClassSpec) error {
	return errors.New("this backend refuses this class")
}
func (rejectingKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, errors.New("not constructible in tests")
}

func init() { sandboxregistry.Register(rejectingKind{}) }

// noSessionUnpackKind supports every class-level feature but NOT
// FeatureUnpackMounts — the case a session-provenance tarGz mount (a staged
// skill, or the legacy SkillBundles fallback) must be refused for, even
// though the CLASS itself asked for nothing the kind can't do.
type noSessionUnpackKind struct{}

func (noSessionUnpackKind) Name() string { return "test-no-session-unpack" }
func (noSessionUnpackKind) Supports(f sandboxkinds.Feature) bool {
	return f != sandboxkinds.FeatureUnpackMounts
}
func (noSessionUnpackKind) WorkspaceDomain() string { return sandboxkinds.DomainKubernetesPVC }
func (noSessionUnpackKind) ValidateClass(spiceboxv1alpha1.SpiceboxClassSpec) error {
	return nil
}
func (noSessionUnpackKind) NewRuntime(sandboxkinds.Deps) (sandboxkinds.Runtime, error) {
	return nil, errors.New("not constructible in tests")
}

func init() { sandboxregistry.Register(noSessionUnpackKind{}) }

// The tier override can replace the kind a class declared, so the spiceboxclass
// controller's validation only ever saw the DECLARED kind. This is the gate on
// the kind that actually runs.
func TestValidateResolvedSandbox(t *testing.T) {
	cases := []struct {
		name               string
		class              spiceboxv1alpha1.SpiceboxClassSpec
		sessionMounts      []spiceboxv1alpha1.SpiceboxMount
		legacySkillBundles bool
		wantErr            string // "" means the class must be accepted
	}{
		{
			name:  "unset kind resolves to the built-in pod backend and is accepted",
			class: spiceboxv1alpha1.SpiceboxClassSpec{Image: "demo.invalid/img"},
		},
		{
			name: "pod backend accepts a feature it supports (toolchain overlay)",
			class: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:      "demo.invalid/img",
				Toolchains: []string{"demo-toolchain"},
			},
		},
		{
			name: "unregistered override kind: refused, never downgraded to the built-in backend",
			class: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:   "demo.invalid/img",
				Sandbox: spiceboxv1alpha1.SandboxBackend{Kind: "no-such-backend"},
			},
			wantErr: `sandbox kind "no-such-backend" is not a registered sandbox backend`,
		},
		{
			name: "backend's own ValidateClass veto is reached and surfaced",
			class: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:   "demo.invalid/img",
				Sandbox: spiceboxv1alpha1.SandboxBackend{Kind: "test-rejecting"},
			},
			wantErr: `sandbox kind "test-rejecting" rejected the class: this backend refuses this class`,
		},
		{
			// The class itself asks for nothing the kind can't do (no
			// class.mounts, no class.toolchains) -- only the SESSION's staged-
			// skill mount does. Before ValidateSessionAgainstKind existed, this
			// case passed validateResolvedSandbox and went on to render a pod
			// the kind could not unpack.
			name: "session-provenance tarGz mount against a kind lacking unpack: rejected even though the class asks for nothing unusual",
			class: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:   "demo.invalid/img",
				Sandbox: spiceboxv1alpha1.SandboxBackend{Kind: "test-no-session-unpack"},
			},
			sessionMounts: []spiceboxv1alpha1.SpiceboxMount{{
				Name:      "staged-skill",
				MountPath: "/skills/staged-skill",
				Format:    spiceboxv1alpha1.MountFormatTarGz,
				Source: spiceboxv1alpha1.MountSource{
					ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
				},
			}},
			wantErr: `session mounts require the "unpack-mounts" capability, which sandbox kind "test-no-session-unpack" does not support`,
		},
		{
			// Mirrors the case above but via the DEPRECATED SkillBundles fallback
			// instead of native session.Mounts -- podspec.Build converts either
			// into the same tarGz mount, so the gate must too.
			name: "legacy SkillBundles fallback against a kind lacking unpack: rejected",
			class: spiceboxv1alpha1.SpiceboxClassSpec{
				Image:   "demo.invalid/img",
				Sandbox: spiceboxv1alpha1.SandboxBackend{Kind: "test-no-session-unpack"},
			},
			legacySkillBundles: true,
			wantErr:            `session mounts require the "unpack-mounts" capability, which sandbox kind "test-no-session-unpack" does not support`,
		},
		{
			name: "session-provenance tarGz mount accepted by a kind that supports unpack",
			class: spiceboxv1alpha1.SpiceboxClassSpec{
				Image: "demo.invalid/img",
			},
			sessionMounts: []spiceboxv1alpha1.SpiceboxMount{{
				Name:      "staged-skill",
				MountPath: "/skills/staged-skill",
				Format:    spiceboxv1alpha1.MountFormatTarGz,
				Source: spiceboxv1alpha1.MountSource{
					ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-cm"},
				},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResolvedSandbox(tc.class, tc.sessionMounts, tc.legacySkillBundles)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
