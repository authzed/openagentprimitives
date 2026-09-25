package sandboxkinds

import (
	"fmt"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ValidateClassAgainstKind rejects a class asking for something its resolved
// sandbox backend cannot do.
//
// It lives here rather than in pkg/apis/v1alpha1 because it needs both a Kind
// and a SpiceboxClassSpec; the reverse direction would be an import cycle. The
// kind is a parameter rather than a registry lookup so this package never
// imports its own registry and a consumer links only the backends it
// blank-imports. Each check names the capability and the backend, so the
// resulting condition tells an operator what to change.
func ValidateClassAgainstKind(k Kind, spec v1alpha1.SpiceboxClassSpec) error {
	required := []struct {
		feature Feature
		asked   bool
		field   string
	}{
		{FeatureConfigMapMounts, len(spec.Mounts) > 0, "spec.mounts"},
		{FeatureToolchainOverlay, len(spec.Toolchains) > 0, "spec.toolchains"},
		{FeatureUnpackMounts, hasUnpackMount(spec.Mounts), "spec.mounts"},
	}
	for _, r := range required {
		if r.asked && !k.Supports(r.feature) {
			return fmt.Errorf("%s requires the %q capability, which sandbox kind %q does not support",
				r.field, r.feature, k.Name())
		}
	}
	return nil
}

// ValidateSessionAgainstKind rejects a SESSION asking for something its
// resolved sandbox backend cannot do, mirroring ValidateClassAgainstKind but
// over session-PROVENANCE mounts instead of the class's authored ones.
//
// This is a separate gate because a class's own spec.mounts is not the only
// source of mounts a session's pod ends up with: BuildBundleSession
// (pkg/controllers/agentsession/bundles.go) converts every staged skill into
// a SpiceboxMount on SpiceboxSessionSpec.Mounts, always tarGz-formatted, and
// ValidateClassAgainstKind never sees it — it only ever runs against the
// class spec. Both shipped kinds (pod, agent-sandbox) declare every Feature
// today, so nothing observably breaks yet, but a future kind that does not
// support FeatureUnpackMounts would silently receive a tarGz session mount it
// cannot unpack and render a pod that fails to come up.
//
// legacySkillBundles is true when the session also carries the DEPRECATED
// SpiceboxSessionSpec.SkillBundles fallback: podspec.Build always converts
// that into a tarGz mount too (see its own doc comment in
// pkg/platform/podspec/builder.go), so that field's mere presence — even
// with sessionMounts empty — also demands FeatureUnpackMounts.
func ValidateSessionAgainstKind(k Kind, sessionMounts []v1alpha1.SpiceboxMount, legacySkillBundles bool) error {
	required := []struct {
		feature Feature
		asked   bool
		field   string
	}{
		{FeatureConfigMapMounts, len(sessionMounts) > 0 || legacySkillBundles, "session mounts"},
		{FeatureUnpackMounts, hasUnpackMount(sessionMounts) || legacySkillBundles, "session mounts"},
	}
	for _, r := range required {
		if r.asked && !k.Supports(r.feature) {
			return fmt.Errorf("%s require the %q capability, which sandbox kind %q does not support",
				r.field, r.feature, k.Name())
		}
	}
	return nil
}

// hasUnpackMount reports whether any mount in mounts needs expansion (a
// Format that is set and not raw). Derived from the mounts themselves rather
// than from a count, so a future non-raw format is covered without editing
// this predicate. Shared by ValidateClassAgainstKind (class-authored mounts)
// and ValidateSessionAgainstKind (session-provenance mounts): the pod
// builder's applyMounts (pkg/platform/podspec/mounts.go) runs the identical
// tarGz-unpack machinery regardless of which provenance a mount came from, so
// neither validation gate should either.
func hasUnpackMount(mounts []v1alpha1.SpiceboxMount) bool {
	for _, m := range mounts {
		if m.Format != "" && m.Format != v1alpha1.MountFormatRaw {
			return true
		}
	}
	return false
}
