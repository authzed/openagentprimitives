//go:build integration

package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// packedDemoAgent loads the shared oaptest bundle fixture, packs it, and
// unpacks it again — exercising Install against a Bundle that came through
// the same Pack/Unpack round trip a real `oap agent install` would see (a
// registry pull or a local .oap file), not a bare FromFolder Bundle.
func packedDemoAgent(t *testing.T) *oap.Bundle {
	t.Helper()
	folder, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err, "load demo-agent fixture folder")
	packed, err := oap.Pack(folder)
	require.NoError(t, err, "pack demo-agent fixture")
	b, err := oap.Unpack(packed)
	require.NoError(t, err, "unpack demo-agent fixture")
	return b
}

// resolveDemoAgent runs install.Resolve non-interactively for the demo-agent
// fixture's two questions. "demoToken" (the secret question) is answered via
// --set; "repos" is left unanswered so Resolve's Question.Default fallback
// (its manifest default ["demo-org/demo-repo"]) fills it — exercising that path
// through the real install pipeline.
func resolveDemoAgent(t *testing.T, b *oap.Bundle, tokenValue string) (oap.Answers, []install.SecretSpec) {
	t.Helper()
	answers, secrets, err := install.Resolve(b.Manifest.Questions, "", map[string]string{
		"demoToken": tokenValue,
	}, false)
	require.NoError(t, err, "resolve demo-agent questions non-interactively")
	return answers, secrets
}

func TestInstall_AppliesAgentClassAndSecret_ReinstallIsIdempotent(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-basic"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	b := packedDemoAgent(t)
	answers, secrets := resolveDemoAgent(t, b, "fake-token-value")
	require.Len(t, secrets, 1, "the demoToken question must produce exactly one SecretSpec")

	opts := install.InstallOpts{Namespace: ns}
	result, err := install.Install(ctx, env.Client, b, answers, secrets, opts)
	require.NoError(t, err, "first Install")
	assert.Equal(t, "demo-class", result.Name, "install name defaults to the bundled AgentClass's own name")
	assert.Equal(t, 1, result.SecretsCreated)
	assert.Contains(t, result.AppliedKinds, "AgentClass")

	var ac v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-class"}, &ac), "AgentClass exists")
	assert.Equal(t, "demo-class", ac.Labels["app.kubernetes.io/instance"], "app.kubernetes.io/instance install label")
	assert.Equal(t, "demo-class", ac.Labels["agentprimitives.authzed.com/oap-install"], "oap-install label")
	require.NotEmpty(t, ac.Spec.GetSlots(), "manifest's github_repo_url authz.slots entry must be present")
	assert.Equal(t, []string{"demo-org/demo-repo"}, ac.Spec.GetSlots()[0].Defaults, "unanswered 'repos' question keeps its default, overlaid by oap.Apply")

	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-pat"}, &sec), "Secret created from the demoToken SecretSpec")
	assert.Equal(t, "fake-token-value", string(sec.Data["token"]), "secret value lands under the manifest's declared key")
	assert.Equal(t, "demo-class", sec.Labels["app.kubernetes.io/instance"], "the Secret carries the same install labels as the CRs")

	// Re-install the same bundle+answers: SSA-apply is idempotent — this must
	// succeed without creating a second AgentClass or Secret.
	b2 := packedDemoAgent(t)
	answers2, secrets2 := resolveDemoAgent(t, b2, "fake-token-value")
	result2, err := install.Install(ctx, env.Client, b2, answers2, secrets2, opts)
	require.NoError(t, err, "second Install of the identical bundle/answers must be a no-op success, not an error")
	assert.Equal(t, result.Name, result2.Name)

	var acList v1alpha1.AgentClassList
	require.NoError(t, env.Client.List(ctx, &acList, client.InNamespace(ns)))
	assert.Len(t, acList.Items, 1, "re-install must not duplicate the AgentClass")

	var secList corev1.SecretList
	require.NoError(t, env.Client.List(ctx, &secList, client.InNamespace(ns), client.MatchingLabels{"agentprimitives.authzed.com/oap-install": "demo-class"}))
	assert.Len(t, secList.Items, 1, "re-install must not duplicate the Secret")
}

// TestInstall_StampsOapSourceAnnotationOnAgentClass pins that Install with a
// registry-style source in InstallOpts stamps instance.AnnotationOapSource onto
// the applied AgentClass CR — and ONLY the AgentClass, never the co-applied
// Secret — parsing back to the exact ref/digest/sourceKind passed in plus the
// manifest's declared version.
func TestInstall_StampsOapSourceAnnotationOnAgentClass(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-source-annotation"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	b := packedDemoAgent(t)
	answers, secrets := resolveDemoAgent(t, b, "fake-token-value")

	opts := install.InstallOpts{
		Namespace:    ns,
		SourceKind:   "registry",
		SourceRef:    "ghcr.io/example/demo-agent:1",
		SourceDigest: "sha256:deadbeef1234",
	}
	result, err := install.Install(ctx, env.Client, b, answers, secrets, opts)
	require.NoError(t, err, "Install with a registry-style source")

	var ac v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: result.Name}, &ac), "AgentClass exists")
	raw, ok := ac.Annotations[instance.AnnotationOapSource]
	require.True(t, ok, "AgentClass must carry the oap-source annotation")

	src, err := instance.ParseOapSource(raw)
	require.NoError(t, err, "annotation value must parse as OapSource")
	assert.Equal(t, "ghcr.io/example/demo-agent:1", src.Ref)
	assert.Equal(t, "sha256:deadbeef1234", src.Digest)
	assert.Equal(t, "registry", src.SourceKind)
	assert.Equal(t, b.Manifest.Agent.Version, src.Version, "Version must come from the manifest's declared agent.version")

	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-pat"}, &sec))
	assert.NotContains(t, sec.Annotations, instance.AnnotationOapSource, "only the AgentClass CR is annotated, not every applied CR")
}

// TestInstall_OapSourceAnnotationIsSSAIdempotent is the regression guard for
// the SSA-idempotency fix: the oap-source annotation must be a pure function
// of the bundle (no install timestamp), so re-installing the SAME bundle from
// the SAME source leaves the annotation value BYTE-IDENTICAL — and, more
// broadly, leaves every field this install's field manager applies untouched,
// resourceVersion included. If a wall-clock (or any other volatile value)
// ever leaks back into an applied field, the two reads diverge and this
// fails.
//
// # Why it sleeps
//
// The sleep forces the two applies into DIFFERENT wall-clock seconds. This
// used to matter for a second reason beyond just exercising the annotation:
// `AgentClassSpec.Authz.Slots` is `+listType=atomic`, so SSA treats the whole
// list as one value, and `AuthzSlot.Membership` carries
// `+kubebuilder:default=frozen`. A bundle that leaves membership unset (the
// normal case, including this fixture) used to apply a slot list that
// REPLACED the live one — atomic lists merge as a whole, not element by
// element — with one missing a field the apiserver had already defaulted
// onto the stored object. The field manager recorded that as a change even
// though the apiserver re-defaulted membership on the way to storage and the
// stored spec ended up identical, so `managedFields[].time` moved on
// `time.Now()` while `generation` never did. `metav1.Time` serializes at
// one-second granularity and the storage layer skips a write whose
// serialized bytes are unchanged, so this bumped resourceVersion on about one
// run in six — precisely when the two applies straddled a second boundary.
// The sleep makes that boundary-straddle deterministic rather than
// occasional, which is what makes this test a reliable regression guard
// rather than a flake either way.
//
// install.Install now completes each slot's membership with
// v1alpha1.AuthzSlotMembershipDefault before the SSA-apply loop
// (completeAuthzSlotDefaults, apply.go) whenever a bundle leaves it
// unset, which is what the CRD would have defaulted onto the stored object
// anyway — so the applied payload matches what the apiserver stores, the
// merge is a true no-op, and resourceVersion no longer moves across the
// straddle. The CRD-level alternative (a list-map key on `slots`) remains a
// separate, larger decision this fix does not need to make.
func TestInstall_OapSourceAnnotationIsSSAIdempotent(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const ns = "oap-install-source-idempotent"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	opts := install.InstallOpts{
		Namespace:    ns,
		SourceKind:   "registry",
		SourceRef:    "ghcr.io/example/demo-agent:1",
		SourceDigest: "sha256:deadbeef1234",
	}

	b1 := packedDemoAgent(t)
	answers1, secrets1 := resolveDemoAgent(t, b1, "fake-token-value")
	result, err := install.Install(ctx, env.Client, b1, answers1, secrets1, opts)
	require.NoError(t, err, "first Install")

	key := types.NamespacedName{Namespace: ns, Name: result.Name}
	ac1 := getAgentClassUnstructured(t, ctx, env.Client, key)
	first := ac1.GetAnnotations()[instance.AnnotationOapSource]
	require.NotEmpty(t, first, "AgentClass must carry the oap-source annotation after first install")

	// Land the second apply in a different wall-clock second (see the doc
	// comment): without this the two applies share a second about five runs in
	// six, and any timestamp drift they introduce is invisible.
	time.Sleep(1500 * time.Millisecond)

	// Re-install the identical bundle from the identical source.
	b2 := packedDemoAgent(t)
	answers2, secrets2 := resolveDemoAgent(t, b2, "fake-token-value")
	_, err = install.Install(ctx, env.Client, b2, answers2, secrets2, opts)
	require.NoError(t, err, "second Install of the identical bundle/source")

	ac2 := getAgentClassUnstructured(t, ctx, env.Client, key)
	second := ac2.GetAnnotations()[instance.AnnotationOapSource]

	assert.Equal(t, first, second, "re-install must leave the oap-source annotation byte-identical (no timestamp drift)")
	assert.Equal(t, ac1.GetGeneration(), ac2.GetGeneration(),
		"a re-install of the identical bundle must not churn the spec, so generation must not move")
	assert.Equal(t, ac1.GetResourceVersion(), ac2.GetResourceVersion(),
		"a re-install of the identical bundle must be a true SSA no-op, so resourceVersion must "+
			"not move even across a wall-clock second boundary")
	assert.Equal(t, apiserverBookkeepingStripped(t, ac1), apiserverBookkeepingStripped(t, ac2),
		"every field the installer applies must survive a re-install byte-identical; only the "+
			"apiserver's own managedFields timestamps may differ")
}

// getAgentClassUnstructured reads the AgentClass as unstructured so the whole
// stored object — not just the fields a typed struct happens to expose — can
// be compared across two installs.
func getAgentClassUnstructured(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName) *unstructured.Unstructured {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("AgentClass"))
	require.NoError(t, c.Get(ctx, key, got), "get AgentClass %s", key)
	return got
}

// apiserverBookkeepingStripped renders obj as indented JSON (so a failure
// prints a readable diff) with the two fields the apiserver owns and moves on
// its own removed: metadata.resourceVersion and each managedFields entry's
// time. Everything else — labels, annotations, the whole spec, and every
// manager's field SET — is left in, because a volatile value in an applied
// field would show up there.
func apiserverBookkeepingStripped(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	cp := obj.DeepCopy()
	cp.SetResourceVersion("")
	mf := cp.GetManagedFields()
	for i := range mf {
		mf[i].Time = nil
	}
	cp.SetManagedFields(mf)
	raw, err := json.MarshalIndent(cp.Object, "", "  ")
	require.NoError(t, err, "marshal stored AgentClass for comparison")
	return string(raw)
}

// TestInstall_DefaultsOapSourceToFileWhenSourceKindUnset covers InstallOpts's
// safe default: a caller that never sets SourceKind/SourceRef/SourceDigest still
// gets a well-formed annotation — SourceKind "file", empty Ref — rather than an
// ambiguous/empty SourceKind landing on the CR.
func TestInstall_DefaultsOapSourceToFileWhenSourceKindUnset(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-source-default"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	b := packedDemoAgent(t)
	answers, secrets := resolveDemoAgent(t, b, "fake-token-value")

	result, err := install.Install(ctx, env.Client, b, answers, secrets, install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "Install without any Source* opts set")

	var ac v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: result.Name}, &ac))
	raw, ok := ac.Annotations[instance.AnnotationOapSource]
	require.True(t, ok, "AgentClass must still carry the oap-source annotation")

	src, err := instance.ParseOapSource(raw)
	require.NoError(t, err)
	assert.Equal(t, "file", src.SourceKind, "unset SourceKind defaults to \"file\"")
	assert.Empty(t, src.Ref, "a file-defaulted source has no ref")
}

func TestInstall_WithNameOpt_PrefixesNamesAndCoexistsWithUnprefixedInstall(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-named"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	// First, an unprefixed install (opts.Name == "").
	b1 := packedDemoAgent(t)
	answers1, secrets1 := resolveDemoAgent(t, b1, "fake-token-value")
	_, err := install.Install(ctx, env.Client, b1, answers1, secrets1, install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "unprefixed install")

	// Second, the SAME bundle installed again under a distinct instance name —
	// instance.Rename must prefix every bundled CR's name so the two installs
	// coexist rather than colliding on "demo-class".
	b2 := packedDemoAgent(t)
	answers2, secrets2 := resolveDemoAgent(t, b2, "fake-token-value")
	result2, err := install.Install(ctx, env.Client, b2, answers2, secrets2, install.InstallOpts{Name: "pm2", Namespace: ns})
	require.NoError(t, err, "prefixed install must coexist with the unprefixed one, not conflict")
	assert.Equal(t, "pm2", result2.Name)
	assert.Contains(t, result2.AppliedKinds, "AgentClass")

	var original v1alpha1.AgentClass
	assert.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "demo-class"}, &original), "original unprefixed AgentClass is still present")

	var prefixed v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pm2-demo-class"}, &prefixed), "prefixed AgentClass (pm2-demo-class) coexists alongside it")
	assert.Equal(t, "pm2", prefixed.Labels["app.kubernetes.io/instance"], "prefixed install's label is the --name value, not the CR's own name")
}

// bundleFromCRs builds an in-memory Bundle whose Manifests stream is the
// marshaled multi-doc YAML of objs. The manifest has no questions, so
// Bundle.Validate passes on the CR stream alone. Used by tests that need a
// bundle carrying a specific CR shape (a cluster-scoped CR, an AgentIdentity
// with a secretRef) the demo-agent folder fixture doesn't provide.
func bundleFromCRs(t *testing.T, objs ...any) *oap.Bundle {
	t.Helper()
	var buf bytes.Buffer
	for i, o := range objs {
		if i > 0 {
			buf.WriteString("\n---\n")
		}
		data, err := yaml.Marshal(o)
		require.NoError(t, err, "marshal bundled CR")
		buf.Write(data)
	}
	return &oap.Bundle{
		Manifest:  &oap.Manifest{OapFormatVersion: "1", Agent: oap.Agent{Name: "test-agent", Version: "1.0.0"}},
		Manifests: buf.Bytes(),
	}
}

// minimalAgentClass is a structurally-valid AgentClass CR (systemPrompt is the
// one required spec field the CRD enforces) for bundle fixtures.
func minimalAgentClass(name string) map[string]any {
	return map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentClass",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"systemPrompt": map[string]any{"inline": "fake test agent"},
		},
	}
}

// TestInstall_BundledClusterScopedCR_ConflictsWithSharedInfra is the C1
// regression test: a bundle carrying a cluster-scoped SpiceboxToolkit whose
// name collides with a pre-existing shared toolkit NOT managed by this install
// must hard-conflict — never force-overwrite the shared infra via
// ForceOwnership — and must apply NOTHING (the AgentClass never lands).
func TestInstall_BundledClusterScopedCR_ConflictsWithSharedInfra(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-conflict"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	// Pre-existing shared toolkit, created by something OTHER than this install
	// (no oap-install label). fakeToolkit is defined in
	// clusterdeps_integration_test.go (same install_test package).
	shared := fakeToolkit("shared-toolkit")
	require.NoError(t, env.Client.Create(ctx, shared), "create pre-existing shared SpiceboxToolkit")

	// A bundle carrying a same-named cluster-scoped SpiceboxToolkit. The
	// manifest does NOT declare it under requires.clusterDeps, so it is not an
	// intended shared dep — installing it would seize the shared toolkit.
	bundledToolkit := map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "SpiceboxToolkit",
		"metadata":   map[string]any{"name": "shared-toolkit"},
		"spec":       map[string]any{"name": "shared-toolkit", "toolkitRevision": "v2-bundle"},
	}
	b := bundleFromCRs(t, minimalAgentClass("conflict-agent"), bundledToolkit)

	_, err := install.Install(ctx, env.Client, b, oap.Answers{}, nil, install.InstallOpts{Namespace: ns})
	require.Error(t, err, "a bundled cluster-scoped CR colliding with unmanaged shared infra must hard-conflict")
	assert.Contains(t, err.Error(), "refusing to overwrite shared infra")
	assert.Contains(t, err.Error(), "SpiceboxToolkit/shared-toolkit")

	// The shared toolkit's spec must be untouched (not seized).
	var got v1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: "shared-toolkit"}, &got))
	assert.NotContains(t, got.Labels, install.FieldManager, "shared toolkit must not carry this install's field manager as a label")
	assert.Equal(t, "/usr/bin/fake-tool", got.Spec.Target.Binary, "shared toolkit spec must be untouched by the aborted install")

	// The conflict aborts before ANY write — the AgentClass never lands.
	var ac v1alpha1.AgentClass
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "conflict-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(err), "the AgentClass must NOT have been applied when the install conflicts")
}

// TestInstall_BundledNamespacedResource_ConflictsWithForeignConfigMap is the
// residual-confused-deputy regression: the ownership guard must cover
// NAMESPACED bundled resources too, not just cluster-scoped ones. A bundle
// carrying a ConfigMap whose namespace+name collide with a pre-existing foreign
// ConfigMap (no oap-install label) must hard-conflict, leave that ConfigMap's
// data untouched, and apply nothing. Pre-fix, Install force-applied every
// namespaced CR with ForceOwnership and would have seized it.
func TestInstall_BundledNamespacedResource_ConflictsWithForeignConfigMap(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-cm-conflict"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	// A pre-existing foreign ConfigMap, created by something OTHER than this
	// install (no oap-install label) — stands in for the audit trust-root
	// "publisher-keys" ConfigMap a crafted bundle would try to seize.
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "publisher-keys", Namespace: ns},
		Data:       map[string]string{"trusted-key": "original-value"},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign ConfigMap")

	// A bundle carrying a same-named ConfigMap (attacker-controlled data).
	bundledCM := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "publisher-keys"},
		"data":       map[string]any{"trusted-key": "attacker-value"},
	}
	b := bundleFromCRs(t, minimalAgentClass("cm-conflict-agent"), bundledCM)

	_, err := install.Install(ctx, env.Client, b, oap.Answers{}, nil, install.InstallOpts{Namespace: ns})
	require.Error(t, err, "a bundled namespaced ConfigMap colliding with a foreign one must hard-conflict")
	assert.Contains(t, err.Error(), "refusing to overwrite")
	assert.Contains(t, err.Error(), "ConfigMap "+ns+"/publisher-keys")

	// The foreign ConfigMap's data must be UNCHANGED (not seized).
	var got corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "publisher-keys"}, &got))
	assert.Equal(t, "original-value", got.Data["trusted-key"], "foreign ConfigMap data must be untouched by the aborted install")
	assert.NotContains(t, got.Labels, instance.LabelInstall, "foreign ConfigMap must not have been stamped by this install")

	// The conflict aborts before ANY write — the AgentClass never lands.
	var ac v1alpha1.AgentClass
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "cm-conflict-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(err), "the AgentClass must NOT have been applied when the install conflicts")
}

// TestInstall_BundledConfigMap_FreshAndReinstall confirms the namespaced
// ownership guard does not over-reject: a bundle carrying a ConfigMap installs
// cleanly when the ConfigMap is absent (fresh) and re-installs idempotently
// once it carries this install's label (converge, no conflict).
func TestInstall_BundledConfigMap_FreshAndReinstall(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-cm-fresh"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	bundledCM := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "agent-prompt"},
		"data":       map[string]any{"prompt": "hello"},
	}
	build := func() *oap.Bundle { return bundleFromCRs(t, minimalAgentClass("cm-fresh-agent"), bundledCM) }

	// Fresh install: ConfigMap absent → created and stamped.
	_, err := install.Install(ctx, env.Client, build(), oap.Answers{}, nil, install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "fresh install of a bundle carrying a ConfigMap must succeed")

	var cm corev1.ConfigMap
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-prompt"}, &cm))
	assert.Equal(t, "cm-fresh-agent", cm.Labels[instance.LabelInstall], "the ConfigMap carries this install's label")

	// Re-install: ConfigMap now carries our label → converge, not conflict.
	_, err = install.Install(ctx, env.Client, build(), oap.Answers{}, nil, install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "re-install of the same bundle must be idempotent, not a self-conflict")
}

// TestInstall_SynthesizedSecret_ConflictsWithForeignSecret is the last
// confused-deputy vector: the Secrets install synthesizes from answered secret
// questions (SecretSpec) are force-applied too, so a bundle declaring a
// secret-question whose target Secret NAME collides with a foreign Secret in
// the target namespace would overwrite that Secret's data with the answered
// value. The synthesized-Secret ownership guard must hard-conflict, leave the
// foreign Secret's data untouched, and apply nothing.
func TestInstall_SynthesizedSecret_ConflictsWithForeignSecret(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-secret-conflict"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	// A pre-existing FOREIGN Secret (no oap-install label) — stands in for any
	// secret the attacker's answer value would try to seize.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("original-secret")},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign Secret")

	// A bundle whose synthesized Secret targets the same name, with the
	// attacker's answer value.
	b := bundleFromCRs(t, minimalAgentClass("secret-conflict-agent"))
	secrets := []install.SecretSpec{{Name: "app-secret", Key: "token", Value: "attacker-secret"}}

	_, err := install.Install(ctx, env.Client, b, oap.Answers{}, secrets, install.InstallOpts{Namespace: ns})
	require.Error(t, err, "a synthesized Secret colliding with a foreign Secret must hard-conflict")
	assert.Contains(t, err.Error(), "refusing to overwrite")
	assert.Contains(t, err.Error(), "Secret "+ns+"/app-secret")

	// The foreign Secret's data must be UNCHANGED (not seized).
	var got corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app-secret"}, &got))
	assert.Equal(t, "original-secret", string(got.Data["token"]), "foreign Secret data must be untouched by the aborted install")
	assert.NotContains(t, got.Labels, instance.LabelInstall, "foreign Secret must not have been stamped by this install")

	// The conflict aborts before ANY write — the AgentClass never lands.
	var ac v1alpha1.AgentClass
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "secret-conflict-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(err), "the AgentClass must NOT have been applied when the install conflicts")
}

// TestInstall_SynthesizedSecret_FreshAndReinstall confirms the synthesized-
// Secret guard does not over-reject: a fresh install creates the Secret and a
// re-install (same install name → own label) converges the value rather than
// self-conflicting.
func TestInstall_SynthesizedSecret_FreshAndReinstall(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-secret-fresh"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	build := func() *oap.Bundle { return bundleFromCRs(t, minimalAgentClass("secret-fresh-agent")) }
	spec := func(v string) []install.SecretSpec {
		return []install.SecretSpec{{Name: "fresh-secret", Key: "token", Value: v}}
	}

	// Fresh install: Secret absent → created and stamped.
	_, err := install.Install(ctx, env.Client, build(), oap.Answers{}, spec("v1"), install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "fresh install synthesizing a Secret must succeed")

	var sec corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "fresh-secret"}, &sec))
	assert.Equal(t, "secret-fresh-agent", sec.Labels[instance.LabelInstall], "the synthesized Secret carries this install's label")

	// Re-install with a new value: Secret now carries our label → converge.
	_, err = install.Install(ctx, env.Client, build(), oap.Answers{}, spec("v2"), install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "re-install of an install's own Secret must converge, not self-conflict")

	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "fresh-secret"}, &sec))
	assert.Equal(t, "v2", string(sec.Data["token"]), "re-install converges the Secret value")
}

// TestInstall_NameOpt_IsolatesSecretsAndRewritesSecretRef is the I2
// regression test: two --name installs of a bundle whose AgentIdentity names a
// created Secret must each get their OWN prefixed Secret (distinct values, no
// clobber), and each AgentIdentity's static.secretRef.name must be rewritten
// to its instance's prefixed Secret.
func TestInstall_NameOpt_IsolatesSecretsAndRewritesSecretRef(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-secret-iso"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	identity := map[string]any{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata":   map[string]any{"name": "app-identity"},
		"spec": map[string]any{
			"credentials": []any{
				map[string]any{
					"name":   "cred",
					"type":   "static",
					"static": map[string]any{"secretRef": map[string]any{"name": "app-secret", "key": "token"}},
				},
			},
		},
	}

	build := func() *oap.Bundle { return bundleFromCRs(t, minimalAgentClass("iso-agent"), identity) }

	// Two instances, distinct secret values. The SecretSpec name matches the
	// AgentIdentity's secretRef so Rename links them under the prefix.
	spec := func(v string) []install.SecretSpec {
		return []install.SecretSpec{{Name: "app-secret", Key: "token", Value: v}}
	}

	_, err := install.Install(ctx, env.Client, build(), oap.Answers{}, spec("value-pm2"), install.InstallOpts{Name: "pm2", Namespace: ns})
	require.NoError(t, err, "first named install")
	_, err = install.Install(ctx, env.Client, build(), oap.Answers{}, spec("value-pm3"), install.InstallOpts{Name: "pm3", Namespace: ns})
	require.NoError(t, err, "second named install must not clobber the first")

	// Each instance owns a distinct, prefixed Secret with its own value.
	var sec2 corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pm2-app-secret"}, &sec2))
	assert.Equal(t, "value-pm2", string(sec2.Data["token"]))
	var sec3 corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pm3-app-secret"}, &sec3))
	assert.Equal(t, "value-pm3", string(sec3.Data["token"]), "the second install's Secret must be its own, not a clobber of the first")

	// The unprefixed fixed-name Secret must NOT exist — the whole point of I2.
	var bare corev1.Secret
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app-secret"}, &bare)
	assert.True(t, apierrors.IsNotFound(err), "no shared unprefixed Secret must be created under --name")

	// Each AgentIdentity's secretRef points at its OWN prefixed Secret.
	var ai2 v1alpha1.AgentIdentity
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "pm2-app-identity"}, &ai2))
	require.Len(t, ai2.Spec.Credentials, 1)
	require.NotNil(t, ai2.Spec.Credentials[0].Static)
	assert.Equal(t, "pm2-app-secret", ai2.Spec.Credentials[0].Static.SecretRef.Name, "AgentIdentity secretRef must be rewritten to the instance's prefixed Secret")
}

// TestInstall_AdoptForeignAgentClass_SeizesAndConverges is the adopt happy
// path: a stale hand-applied AgentClass that IS the agent being installed.
// Naming it in InstallOpts.Adopt must stamp this install's label on it and
// converge its spec to the bundle's, and report it in Result.Adopted.
func TestInstall_AdoptForeignAgentClass_SeizesAndConverges(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-adopt-ac"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	// A pre-existing, hand-applied AgentClass: no oap-install label, and a
	// system prompt that differs from the bundle's.
	foreign := &v1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "adopt-agent", Namespace: ns},
		Spec: v1alpha1.AgentClassSpec{
			SystemPrompt: v1alpha1.PromptSource{Inline: "hand-applied prompt"},
		},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign AgentClass")

	b := bundleFromCRs(t, minimalAgentClass("adopt-agent"))

	res, err := install.Install(ctx, env.Client, b, oap.Answers{}, nil, install.InstallOpts{
		Namespace: ns,
		Adopt:     []string{"AgentClass/adopt-agent"},
	})
	require.NoError(t, err, "naming the conflicting AgentClass in Adopt must let the install proceed")
	assert.Equal(t, []string{"AgentClass/adopt-agent"}, res.Adopted, "the seized object is reported back to the caller")

	var got v1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "adopt-agent"}, &got))
	assert.Equal(t, "adopt-agent", got.Labels[instance.LabelInstall], "the adopted object now carries this install's label")
	assert.Equal(t, "fake test agent", got.Spec.SystemPrompt.Inline, "the adopted object's spec converged to the bundle's")
}

// TestInstall_AdoptAll_RefusesForeignSecret pins the asymmetry: a blanket
// adopt covers ordinary CRs but NEVER a synthesized Secret, because adopting
// one overwrites its data. The whole install must abort with nothing written.
func TestInstall_AdoptAll_RefusesForeignSecret(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-adoptall-secret"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("original-secret")},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign Secret")

	b := bundleFromCRs(t, minimalAgentClass("adoptall-agent"))
	secrets := []install.SecretSpec{{Name: "app-secret", Key: "token", Value: "bundle-secret"}}

	_, err := install.Install(ctx, env.Client, b, oap.Answers{}, secrets, install.InstallOpts{
		Namespace: ns,
		AdoptAll:  true,
	})
	require.Error(t, err, "a blanket adopt must not cover a foreign Secret")
	// The library error names the object and stays flag-free (see
	// install.ConflictError's doc comment) — cmd/oap's wrapConflictError is
	// what appends "--adopt=Secret/app-secret" for a CLI caller; that CLI-only
	// rendering is pinned in cmd/oap's own test, not here.
	assert.Contains(t, err.Error(), "Secret oap-install-adoptall-secret/app-secret", "the error still names the unadopted object")
	assert.NotContains(t, err.Error(), "--adopt", "the library error must not prescribe a CLI flag")

	var got corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app-secret"}, &got))
	assert.Equal(t, "original-secret", string(got.Data["token"]), "foreign Secret data must be untouched")

	var ac v1alpha1.AgentClass
	err = env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "adoptall-agent"}, &ac)
	assert.True(t, apierrors.IsNotFound(err), "the install must apply NOTHING when a conflict remains")
}

// TestInstall_AdoptNamedSecret_Seizes is the counterpart: naming the Secret
// individually DOES adopt it, overwriting its data with the answered value.
func TestInstall_AdoptNamedSecret_Seizes(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-adopt-secret"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secret", Namespace: ns},
		Data:       map[string][]byte{"token": []byte("original-secret")},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign Secret")

	b := bundleFromCRs(t, minimalAgentClass("adopt-secret-agent"))
	secrets := []install.SecretSpec{{Name: "app-secret", Key: "token", Value: "bundle-secret"}}

	res, err := install.Install(ctx, env.Client, b, oap.Answers{}, secrets, install.InstallOpts{
		Namespace: ns,
		Adopt:     []string{"Secret/app-secret"},
		// The CLI surface: a terminal can put the specific Secret in front of an
		// operator and take a specific answer, which is the individual act
		// adopting one requires. admind leaves this false and is refused --
		// see conflict_secret_adopt_test.go.
		AdoptSecretsAllowed: true,
	})
	require.NoError(t, err, "an individually-named Secret is adoptable from a surface that may adopt one")
	assert.Equal(t, []string{"Secret/app-secret"}, res.Adopted)

	var got corev1.Secret
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "app-secret"}, &got))
	assert.Equal(t, "bundle-secret", string(got.Data["token"]), "the adopted Secret's data is overwritten with the answered value")
	assert.Equal(t, "adopt-secret-agent", got.Labels[instance.LabelInstall])
}

// TestInstall_DeclinedConflict_LeavesClusterDepsUntouched is the ordering
// regression: EnsureClusterDeps writes a metadata-only adoption patch, so it
// must run AFTER the adopt decision. A refused conflict has to leave the
// cluster byte-identical — including a declared shared cluster dep that would
// otherwise have been labelled on the way past.
func TestInstall_DeclinedConflict_LeavesClusterDepsUntouched(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ns = "oap-install-order"
	require.NoError(t, env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}), "create test namespace")

	// A pre-existing shared toolkit the manifest declares as a cluster dep —
	// EnsureClusterDeps would adopt (label) it. fakeToolkit is defined in
	// clusterdeps_integration_test.go (same install_test package).
	dep := fakeToolkit("order-toolkit")
	require.NoError(t, env.Client.Create(ctx, dep), "create pre-existing shared SpiceboxToolkit")

	// A foreign ConfigMap the bundle would seize — the conflict that aborts.
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "order-cm", Namespace: ns},
		Data:       map[string]string{"k": "original"},
	}
	require.NoError(t, env.Client.Create(ctx, foreign), "create pre-existing foreign ConfigMap")

	bundledCM := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "order-cm"},
		"data":       map[string]any{"k": "bundle"},
	}
	b := bundleFromCRs(t, minimalAgentClass("order-agent"), bundledCM)
	b.Manifest.Requires.ClusterDeps = []oap.RequiredClusterDep{{Kind: "SpiceboxToolkit", Name: "order-toolkit"}}

	_, err := install.Install(ctx, env.Client, b, oap.Answers{}, nil, install.InstallOpts{Namespace: ns})
	require.Error(t, err, "the foreign ConfigMap conflict must abort the install")

	var gotDep v1alpha1.SpiceboxToolkit
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Name: "order-toolkit"}, &gotDep))
	assert.NotContains(t, gotDep.Labels, adoptguard.AdoptedLabel,
		"the cluster dep must NOT have been adopted — the ownership decision runs before any write")
}
