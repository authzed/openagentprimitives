package agentsession

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
	skillbundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
)

// e2eClassSpec is the minimal SpiceboxClassSpec podspec.Build needs to render
// a real corev1.Pod -- the same shape podspec_test.go's own baseClass() uses,
// duplicated here rather than imported: podspec_test.go lives in the external
// podspec_test package and its helper is unexported.
func e2eClassSpec() spiceboxv1alpha1.SpiceboxClassSpec {
	return spiceboxv1alpha1.SpiceboxClassSpec{
		Image: "registry.example.invalid/toolbelt:1.0.0",
		Resources: spiceboxv1alpha1.SpiceboxResources{
			CPU:              resource.MustParse("500m"),
			Memory:           resource.MustParse("256Mi"),
			EphemeralStorage: resource.MustParse("100Mi"),
			PidsLimit:        64,
		},
	}
}

// TestSkillStaging_EndToEnd_ClassToRenderedPodSpec_BothProvenances walks the
// FULL pipeline a whole-plan review found nothing else covers in one place:
// AgentClass.spec.skills[] -> resolveAndStageSkillBundles -> BuildBundleSession
// -> podspec.Build -> the actual rendered corev1.Pod, asserting the LEAF PATH
// and the DIGEST for both the native (Mounts) and the deprecated (SkillBundles)
// provenance.
//
// This single test is the one the whole-plan review said would have caught,
// together: a stale comment claiming the two provenances "render the same
// shape for the same skill" (they do not — see the leaf-path assertions
// below); a source-volume renumbering bug that would silently collide two
// mount-src-N volumes (see the ElementsMatch assertion); and a legacy tarGz
// mount extracting with no digest verification at all (see the "no sha256sum"
// assertion). Every other test in this package stops at SpiceboxSessionSpec
// (BuildBundleSession's output) or at a hand-built SpiceboxMount
// (podspec's own builder_test.go) — neither half, alone, can see a defect
// that only shows up once a REAL AgentClass opt-in has been carried all the
// way to the rendered Pod.
func TestSkillStaging_EndToEnd_ClassToRenderedPodSpec_BothProvenances(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"

	// Two sandbox-targeted skills, both opted into staging via "*" — enough to
	// prove per-archive source-volume numbering stays distinct end to end
	// (the resolver-driven equivalent of podspec's own
	// TestBuild_TarGzClassAndSessionMountsShareOneUnpacker, but reached
	// through the real staging pipeline instead of hand-built SpiceboxMounts).
	alpha := demoSkill(t, "alpha", "Alpha skill body.", nil)
	beta := demoSkill(t, "beta", "Beta skill body.", nil)

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-review", Namespace: ns, UID: "uid-e2e"},
	}
	class := sandboxStagingClass(
		sandboxSkill("alpha", demoCanonical("alpha")),
		sandboxSkill("beta", demoCanonical("beta")),
	)

	sch := skillScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess, alpha, beta).Build()
	r := &Reconciler{Client: c, BundleStore: skillbundlemem.New()}

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "staging must not fail on a happy path")
	require.Len(t, resolved, 2, "both sandbox-targeted skills must resolve")

	var resolvedAlpha spiceboxv1alpha1.ResolvedSkillBundle
	for _, rb := range resolved {
		if rb.LocalName == "alpha" {
			resolvedAlpha = rb
		}
	}
	require.NotEmpty(t, resolvedAlpha.LocalName, "alpha must be among the resolved bundles")
	require.NotEmpty(t, resolvedAlpha.ArchiveDigest, "a real archive digest must have been computed")

	classSpec := e2eClassSpec()
	bundle := spiceboxv1alpha1.ToolBundle{Name: "demo-bundle", Class: "demo-class"}

	// The native class ALSO authors its own tarGz mount, unrelated to skill
	// staging. This forces podspec.Build through applyMounts TWICE on one
	// spec — once for class.Mounts (inside BuildClassSpec), once for the two
	// resolver-driven session mounts below — which is exactly the
	// cross-call scenario the source-volume numbering guard below needs: a
	// numbering bug that restarts "mount-src-N" at 0 on the SECOND call only
	// shows up when there IS a second call.
	nativeClassSpec := classSpec
	nativeClassSpec.Mounts = []spiceboxv1alpha1.SpiceboxMount{{
		Name: "class-authored", MountPath: "/opt/class-authored",
		Format: spiceboxv1alpha1.MountFormatTarGz,
		Source: spiceboxv1alpha1.MountSource{
			ConfigMapRef: &corev1.LocalObjectReference{Name: "demo-class-cm"},
		},
	}}

	// ---- Native provenance: AgentClass -> ... -> podspec.Build ----
	nativeSess := BuildBundleSession(sess, bundle, "demo-identity", "", resolved, nil, nil)
	nativePod, err := podspec.Build(nativeSess, nativeClassSpec)
	require.NoError(t, err, "native-provenance Build must succeed")

	nativeMounts := map[string]bool{}
	for _, m := range nativePod.Spec.Containers[0].VolumeMounts {
		nativeMounts[m.MountPath] = true
	}
	assert.True(t, nativeMounts["/skills/alpha"], "native provenance must mount alpha at its LOCAL name")
	assert.True(t, nativeMounts["/skills/beta"], "native provenance must mount beta at its LOCAL name")
	assert.False(t, nativeMounts["/skills/"+resolvedAlpha.MountName],
		"native provenance must NOT mount alpha at its hashed MountName")

	require.Len(t, nativePod.Spec.InitContainers, 1, "one shared mount-unpack init container for both archives")
	nativeIC := nativePod.Spec.InitContainers[0]
	require.Equal(t, "mount-unpack", nativeIC.Name)
	require.Len(t, nativeIC.Command, 3, "sh -c <script>")
	nativeScript := nativeIC.Command[2]

	// DIGEST: the native mount's init-container step must verify against the
	// real archive digest resolveAndStageSkillBundles computed, at the
	// MountName-keyed staging path (Name on the SpiceboxMount is MountName,
	// per BuildBundleSession's doc — never LocalName).
	wantStagingFile := fmt.Sprintf("/staging/%s/bundle.tar.gz", resolvedAlpha.MountName)
	// Single-quoted, not %q: the script is SHELL, and %q is Go-literal quoting
	// that leaves $ and backtick live inside the double-quoted word it was
	// spliced into. podspec.shellQuote is what emits these now.
	assert.Contains(t, nativeScript, fmt.Sprintf("sha256sum '%s'", wantStagingFile),
		"native mount must be digest-checked against its own staged archive")
	assert.Contains(t, nativeScript, fmt.Sprintf(`[ "$got" = '%s' ]`, resolvedAlpha.ArchiveDigest),
		"native mount's digest check must compare against the real ArchiveDigest")

	// MINOR (source-volume numbering): the class-authored archive (staged by
	// the FIRST applyMounts call) and the two resolver-driven session
	// archives (staged by the SECOND) must all get distinct source-volume
	// names — not the session call restarting its numbering at 0 and
	// colliding with the class call's own "mount-src-0". This is the defect
	// a numbering regression would produce without touching a single
	// MountPath assertion above.
	var srcVolNames []string
	for _, m := range nativeIC.VolumeMounts {
		if m.Name != "unpacked" {
			srcVolNames = append(srcVolNames, m.Name)
		}
	}
	assert.ElementsMatch(t, []string{"mount-src-0", "mount-src-1", "mount-src-2"}, srcVolNames,
		"the class-authored archive and both resolver-driven session archives must get distinct, non-restarting source-volume names")

	// ---- Deprecated provenance: the SAME skill, staged via the pre-Mounts
	// SkillBundleMount shape a session created before this plan (and
	// re-hydrated after it) would still carry on spec.skillBundles. ----
	legacySess := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: sess.ObjectMeta,
		Spec: spiceboxv1alpha1.SpiceboxSessionSpec{
			Class: "demo-class",
			SkillBundles: []spiceboxv1alpha1.SkillBundleMount{
				{MountName: resolvedAlpha.MountName, ConfigMapName: resolvedAlpha.ConfigMapName},
			},
		},
	}
	legacyPod, err := podspec.Build(legacySess, classSpec)
	require.NoError(t, err, "legacy-provenance Build must succeed")

	legacyMounts := map[string]bool{}
	for _, m := range legacyPod.Spec.Containers[0].VolumeMounts {
		legacyMounts[m.MountPath] = true
	}
	// LEAF PATH: this is the false equivalence the whole-plan review found —
	// a comment claiming the two provenances "render the same shape for the
	// same skill." For the IDENTICAL skill (same MountName, same
	// ConfigMap), the legacy fallback lands at the hashed MountName, not the
	// LocalName the native path used above.
	assert.True(t, legacyMounts["/skills/"+resolvedAlpha.MountName],
		"legacy provenance mounts the skill at its hashed MountName, not its LocalName")
	assert.False(t, legacyMounts["/skills/alpha"],
		"legacy provenance must NOT reproduce the native provenance's LocalName-addressed leaf path")

	require.Len(t, legacyPod.Spec.InitContainers, 1)
	legacyScript := legacyPod.Spec.InitContainers[0].Command[2]
	// DIGEST: the legacy SkillBundleMount shape carries no Digest field at
	// all, so podspec.Build's fallback conversion cannot populate one --
	// this mount extracts with NO integrity check, unlike its native sibling
	// above. This is the gap the fallback's INFO log (podspec/builder.go)
	// exists to surface; here it is pinned as an observable PodSpec property.
	assert.NotContains(t, legacyScript, "sha256sum",
		"the deprecated SkillBundles fallback must extract unverified (no digest to check against)")
}
