package agentsession

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	skillbundlemem "github.com/authzed/openagentprimitives/pkg/tools/skillbundle/memory"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// digestOf computes the content digest the staging path verifies against: the
// bare sha256 hex of the bundle bytes (matches skillbundle.TarGz). Tests use it
// so the seeded (digest, bytes) pair passes the digest check.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// skillScheme builds a scheme with the core + spicebox types the staging path
// touches (Skills to resolve, ConfigMaps to materialize, the AgentSession owner).
func skillScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return sch
}

const (
	bundledCanonical     = "github.com/exampleorg/examplerepo//skills/bundled@v1.0.0"
	instructionCanonical = "github.com/exampleorg/examplerepo//skills/instruction@v1.0.0"
)

// bundledSkill returns a namespace Skill carrying a bundle at digest, with a
// frontmatter name ("bundled") that matches the AgentSkill.Name every test in
// this file opts it in under, and a real Body so a mismatch in the composed
// SKILL.md would be visible.
func bundledSkill(ns, digest string) *spiceboxv1alpha1.Skill {
	return &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "bundled-skill", Namespace: ns},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: bundledCanonical,
			Description:   "a bundled skill",
			Body:          "Bundled skill body.",
			Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: "bundled"},
			Bundle:        &spiceboxv1alpha1.SkillBundleRef{Digest: digest, CacheKey: digest},
		},
	}
}

func instructionSkill(ns string) *spiceboxv1alpha1.Skill {
	return &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "instruction-skill", Namespace: ns},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: instructionCanonical,
			Description:   "an instruction-only skill",
			Body:          "Instruction-only skill body.",
			Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: "instruction"},
			// Bundle nil → no supporting files, SKILL.md only when staged.
		},
	}
}

// sandboxSkill is a class opt-in targeted at the sandbox — the only target
// skillsToStage ever selects from.
func sandboxSkill(name, ref string) spiceboxv1alpha1.AgentSkill {
	return spiceboxv1alpha1.AgentSkill{Name: name, Ref: ref, Target: spiceboxv1alpha1.SkillTargetSandbox}
}

// sandboxStagingClass builds an AgentClass whose single ToolBundle stages
// every sandbox/both-targeted skill ("*"), so a test only has to vary the
// Skills list to control what actually stages.
func sandboxStagingClass(skills ...spiceboxv1alpha1.AgentSkill) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Skills:      skills,
			ToolBundles: []spiceboxv1alpha1.ToolBundle{{Name: "demo-bundle", StageSkills: []string{"*"}}},
		},
	}
}

func TestResolveAndStageSkillBundles_StagesBundled_ComposesSKILLmdWithFiles_SkipsAgentTargeted(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	archive, digest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("#!/bin/sh\necho ok\n")})
	require.NoError(t, err, "build a real supporting-files archive")

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns, UID: "uid-1"},
	}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(sess, bundledSkill(ns, digest), instructionSkill(ns)).
		Build()

	store := skillbundlemem.New()
	require.NoError(t, store.Put(ctx, digest, archive), "seed the bundle store")

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(
		sandboxSkill("bundled", bundledCanonical),
		spiceboxv1alpha1.AgentSkill{Name: "instruction", Ref: instructionCanonical, Target: spiceboxv1alpha1.SkillTargetAgent},
	)

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "staging must not fail on a happy path")

	// Exactly one resolved bundle: the agent-targeted skill never reaches disk.
	require.Len(t, resolved, 1, "only the sandbox-targeted skill should resolve")
	wantMount := mustSlug(t, bundledCanonical)
	assert.Equal(t, bundledCanonical, resolved[0].CanonicalName)
	assert.Equal(t, "bundled", resolved[0].LocalName, "LocalName is the AgentSkill.Name Claude Code will discover the skill under, not the hashed MountName")
	assert.Equal(t, wantMount, resolved[0].MountName)
	wantCM := skillBundleConfigMapName(sess.Name, wantMount)
	assert.Equal(t, wantCM, resolved[0].ConfigMapName)

	// The ConfigMap carries an AgentSession controller owner ref.
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: wantCM}, &cm),
		"bundle ConfigMap should exist")
	require.Len(t, cm.OwnerReferences, 1, "ConfigMap should carry one owner ref")
	owner := cm.OwnerReferences[0]
	assert.Equal(t, "AgentSession", owner.Kind)
	assert.Equal(t, sess.Name, owner.Name)
	assert.Equal(t, sess.UID, owner.UID)
	require.NotNil(t, owner.Controller)
	assert.True(t, *owner.Controller, "owner ref should be Controller")
	require.NotNil(t, owner.BlockOwnerDeletion)
	assert.True(t, *owner.BlockOwnerDeletion, "owner ref should BlockOwnerDeletion")

	// Independent observation: gunzip+untar the REAL ConfigMap bytes and
	// assert on what's actually inside, not on the inputs used to build it.
	files := untarConfigMap(t, &cm)
	require.Contains(t, files, "SKILL.md", "SKILL.md is composed into every sandbox-staged bundle")
	assert.Contains(t, string(files["SKILL.md"]), "Bundled skill body.")
	assert.Contains(t, string(files["SKILL.md"]), "name: bundled")
	require.Contains(t, files, "scripts/check.sh", "the supporting file rides alongside SKILL.md")
	assert.Equal(t, []byte("#!/bin/sh\necho ok\n"), files["scripts/check.sh"])

	// The agent-targeted skill produced no second ConfigMap.
	var list corev1.ConfigMapList
	require.NoError(t, c.List(ctx, &list))
	assert.Len(t, list.Items, 1, "exactly one ConfigMap total")
}

func TestResolveAndStageSkillBundles_DigestAbsentFromStore_NoConfigMapNoEntryNoError(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	const digest = "sha256:missing"

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns, UID: "uid-2"},
	}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(sess, bundledSkill(ns, digest)).
		Build()

	// Empty store → Get returns ErrNotFound; staging logs + skips, never errors.
	store := skillbundlemem.New()

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(sandboxSkill("bundled", bundledCanonical))

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "a missing bundle must not fail the session")
	assert.Empty(t, resolved, "no bundle resolved when the digest is absent")

	var list corev1.ConfigMapList
	require.NoError(t, c.List(ctx, &list))
	assert.Empty(t, list.Items, "no ConfigMap created for an absent digest")

	// The benign not-cached case is NOT an integrity failure: no condition.
	assert.Nil(t, apimeta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSkillBundlesIntegrity),
		"a not-cached bundle must not surface a SkillBundlesIntegrity condition")
}

// TestResolveAndStageSkillBundles_DigestMismatch_SurfacesIntegrityConditionInstructionOnly
// verifies that when the store returns bytes that do NOT hash to the requested
// digest (corrupt/poisoned cache — an integrity/tamper signal), the bundle is
// not staged (no ConfigMap, no resolved entry, no error) AND the failure is
// surfaced on status via a False SkillBundlesIntegrity condition with the
// DigestMismatch reason — distinct from the benign not-cached case, which sets
// no condition.
func TestResolveAndStageSkillBundles_DigestMismatch_SurfacesIntegrityConditionInstructionOnly(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	// The SkillBundleRef claims this digest...
	digest := digestOf([]byte("expected-bytes"))

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns, UID: "uid-5"},
	}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(sess, bundledSkill(ns, digest)).
		Build()

	// ...but the store hands back different bytes under that digest (a poisoned
	// cache entry). The computed sha256 won't match → staging must skip before
	// ever attempting to untar the (bogus) bytes.
	store := skillbundlemem.New()
	require.NoError(t, store.Put(ctx, digest, []byte("TAMPERED-bytes")), "seed a mismatching bundle")

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(sandboxSkill("bundled", bundledCanonical))

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "a digest mismatch must not fail the session")
	assert.Empty(t, resolved, "no bundle resolved when bytes don't match the digest")

	var list corev1.ConfigMapList
	require.NoError(t, c.List(ctx, &list))
	assert.Empty(t, list.Items, "no ConfigMap created for a mismatching digest")

	// The integrity failure is surfaced on status (visible via kubectl describe),
	// distinguishing it from the benign not-cached case.
	cond := apimeta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSkillBundlesIntegrity)
	require.NotNil(t, cond, "a digest mismatch must surface a SkillBundlesIntegrity condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "integrity condition must be False on mismatch")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionSkillBundleDigestMismatch, cond.Reason)
	assert.Contains(t, cond.Message, bundledCanonical, "message should name the affected skill")
}

func mustSlug(t *testing.T, canonicalName string) string {
	t.Helper()
	n, err := canonical.Parse(canonicalName)
	require.NoError(t, err)
	return n.SafeSlug()
}

// clusterBundledSkill returns a ClusterSkill (cluster-scoped) with a bundle.
func clusterBundledSkill(digest string) *spiceboxv1alpha1.ClusterSkill {
	return &spiceboxv1alpha1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-bundled-skill"},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: bundledCanonical,
			Description:   "a cluster-scoped bundled skill",
			Body:          "Cluster bundled skill body.",
			Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: "bundled"},
			Bundle:        &spiceboxv1alpha1.SkillBundleRef{Digest: digest, CacheKey: digest},
		},
	}
}

// TestResolveAndStageSkillBundles_ClusterSkill_Staged verifies that when only
// a ClusterSkill (no namespace Skill) matches an opted-in canonical name, its
// bundle is staged into a ConfigMap exactly as a namespace Skill would be.
func TestResolveAndStageSkillBundles_ClusterSkill_Staged(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	archive, digest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("cluster-script")})
	require.NoError(t, err)

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-cluster", Namespace: ns, UID: "uid-3"},
	}
	// No namespace Skill — only a ClusterSkill with the same canonical name.
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(sess, clusterBundledSkill(digest)).
		Build()

	store := skillbundlemem.New()
	require.NoError(t, store.Put(ctx, digest, archive), "seed the bundle store")

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(sandboxSkill("bundled", bundledCanonical))

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "staging must not fail when only a ClusterSkill is present")

	require.Len(t, resolved, 1, "cluster-scoped bundled skill should resolve")
	wantMount := mustSlug(t, bundledCanonical)
	assert.Equal(t, bundledCanonical, resolved[0].CanonicalName)
	assert.Equal(t, "bundled", resolved[0].LocalName)
	assert.Equal(t, wantMount, resolved[0].MountName)

	// Confirm the ConfigMap was written with the cluster-skill's supporting
	// file composed alongside SKILL.md.
	wantCM := skillBundleConfigMapName(sess.Name, wantMount)
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: wantCM}, &cm))
	files := untarConfigMap(t, &cm)
	assert.Contains(t, files, "SKILL.md")
	assert.Equal(t, []byte("cluster-script"), files["scripts/check.sh"])
}

// TestResolveAndStageSkillBundles_NSShadowsCluster verifies that when both a
// namespace Skill and a ClusterSkill share a canonical name, the namespace
// Skill wins: exactly one ConfigMap is created and it holds the namespace
// skill's supporting file, not the cluster skill's.
func TestResolveAndStageSkillBundles_NSShadowsCluster(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	nsArchive, nsDigest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("ns-script")})
	require.NoError(t, err)
	clusterArchive, clusterDigest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("cluster-script")})
	require.NoError(t, err)

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-shadow", Namespace: ns, UID: "uid-4"},
	}

	// Both a namespace Skill and a ClusterSkill exist with the same canonical name.
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(sess, bundledSkill(ns, nsDigest), clusterBundledSkill(clusterDigest)).
		Build()

	store := skillbundlemem.New()
	require.NoError(t, store.Put(ctx, nsDigest, nsArchive), "seed namespace bundle")
	require.NoError(t, store.Put(ctx, clusterDigest, clusterArchive), "seed cluster bundle")

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(sandboxSkill("bundled", bundledCanonical))

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "staging must not fail on namespace-shadows-cluster path")

	// Exactly one resolved entry — from the namespace skill.
	require.Len(t, resolved, 1, "namespace skill shadows cluster: only one entry")

	// Exactly one ConfigMap, holding the namespace skill's supporting file.
	wantCM := skillBundleConfigMapName(sess.Name, mustSlug(t, bundledCanonical))
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: wantCM}, &cm))
	files := untarConfigMap(t, &cm)
	assert.Equal(t, []byte("ns-script"), files["scripts/check.sh"],
		"ConfigMap must hold the namespace skill's file, not the cluster skill's")

	var list corev1.ConfigMapList
	require.NoError(t, c.List(ctx, &list))
	assert.Len(t, list.Items, 1, "exactly one ConfigMap total (no extra from cluster skill)")
}

// countingStore wraps a skillbundle.Store and counts Get calls, so a test can
// assert that a re-stage pass did not re-read the bundle bytes.
type countingStore struct {
	inner skillbundle.Store
	gets  int
}

func (s *countingStore) Put(ctx context.Context, digest string, data []byte) error {
	return s.inner.Put(ctx, digest, data)
}

func (s *countingStore) Get(ctx context.Context, digest string) ([]byte, error) {
	s.gets++
	return s.inner.Get(ctx, digest)
}

func (s *countingStore) Has(ctx context.Context, digest string) (bool, error) {
	return s.inner.Has(ctx, digest)
}

// TestResolveAndStageSkillBundles_UnchangedDigest_SkipsRestage pins the
// short-circuit. The call site sits above a 2s requeue with an 8-minute
// bundle-ready deadline, so a booting session runs this path up to ~240 times;
// each pass previously re-read up to 1 MiB from the store, re-hashed it, and
// rewrote the whole ConfigMap through the LIVE uncached APIReader. Nothing about
// a bundle changes between those passes -- the pre-fetch sourceDigest (cheap:
// SKILL.md content + the bundle's own digest) is unchanged, so the composed
// archive would be byte-identical too.
func TestResolveAndStageSkillBundles_UnchangedDigest_SkipsRestage(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	archive, digest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("v1")})
	require.NoError(t, err)

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns, UID: "uid-restage"},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess, bundledSkill(ns, digest)).Build()

	inner := skillbundlemem.New()
	require.NoError(t, inner.Put(ctx, digest, archive), "seed the bundle store")
	store := &countingStore{inner: inner}

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(sandboxSkill("bundled", bundledCanonical))

	// Pass 1: the real stage. The caller stamps the result onto status, exactly
	// as Reconcile does.
	first, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "first stage must succeed")
	require.Len(t, first, 1, "the bundled skill stages on the first pass")
	sess.Status.ResolvedSkillBundles = first
	require.Equal(t, 1, store.gets, "the first pass reads the bundle exactly once")

	// Mark the materialized ConfigMap so a rewrite is detectable: the staging
	// path replaces BinaryData wholesale, so a surviving marker proves no write.
	wantCM := skillBundleConfigMapName(sess.Name, mustSlug(t, bundledCanonical))
	cmKey := types.NamespacedName{Namespace: ns, Name: wantCM}
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, cmKey, &cm), "bundle ConfigMap should exist after the first pass")
	cm.Annotations = map[string]string{"test.agentprimitives/rewrite-marker": "pass-1"}
	require.NoError(t, c.Update(ctx, &cm), "mark the ConfigMap")

	// Pass 2: nothing changed -- same Skill, same digest, same session.
	second, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "second stage must succeed")

	assert.Equal(t, first, second,
		"an unchanged skill must resolve to the identical status entry")
	assert.Equal(t, 1, store.gets,
		"an unchanged skill must not re-read the bundle bytes from the store")

	var after corev1.ConfigMap
	require.NoError(t, c.Get(ctx, cmKey, &after), "bundle ConfigMap should still exist")
	assert.Equal(t, "pass-1", after.Annotations["test.agentprimitives/rewrite-marker"],
		"an unchanged skill must not rewrite the bundle ConfigMap")
}

// TestResolveAndStageSkillBundles_ChangedDigest_Restages is the other half of
// the short-circuit: when the Skill's bundle digest advances, the new bytes
// must actually be staged. A short-circuit that never re-stages would pin the
// session to a stale bundle forever.
func TestResolveAndStageSkillBundles_ChangedDigest_Restages(t *testing.T) {
	ctx := context.Background()
	const ns = "ns"
	oldArchive, oldDigest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("v1")})
	require.NoError(t, err)
	newArchive, newDigest, err := skillbundle.TarGz(map[string][]byte{"scripts/check.sh": []byte("v2")})
	require.NoError(t, err)
	require.NotEqual(t, oldDigest, newDigest)

	sch := skillScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns, UID: "uid-advance"},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(sess, bundledSkill(ns, oldDigest)).Build()

	store := skillbundlemem.New()
	require.NoError(t, store.Put(ctx, oldDigest, oldArchive), "seed the old bundle")
	require.NoError(t, store.Put(ctx, newDigest, newArchive), "seed the new bundle")

	r := &Reconciler{Client: c, BundleStore: store}
	class := sandboxStagingClass(sandboxSkill("bundled", bundledCanonical))

	// Pass 1: stage the OLD content, exactly as a running session would have.
	first, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "staging the old digest must succeed")
	require.Len(t, first, 1)
	sess.Status.ResolvedSkillBundles = first

	// The Skill's bundle advances to the new digest under the running session.
	var live spiceboxv1alpha1.Skill
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "bundled-skill"}, &live))
	live.Spec.Bundle = &spiceboxv1alpha1.SkillBundleRef{Digest: newDigest, CacheKey: newDigest}
	require.NoError(t, c.Update(ctx, &live))

	resolved, err := r.resolveAndStageSkillBundles(ctx, sess, class)
	require.NoError(t, err, "staging an advanced digest must succeed")
	require.Len(t, resolved, 1, "the bundled skill still resolves")
	assert.NotEqual(t, first[0].Digest, resolved[0].Digest, "an advanced bundle must produce a new staged digest")

	wantCM := skillBundleConfigMapName(sess.Name, mustSlug(t, bundledCanonical))
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: wantCM}, &cm))
	files := untarConfigMap(t, &cm)
	assert.Equal(t, []byte("v2"), files["scripts/check.sh"], "the new bytes are staged")
}

// --- Task 7: composing SKILL.md into the staged bundle ---------------------

// demoCanonical returns the fixed canonical name a demoSkill with the given
// frontmatter name resolves under.
func demoCanonical(frontmatterName string) string {
	return "github.com/demo-org/demo-repo//skills/" + frontmatterName + "@v1"
}

// demoSkill builds a namespace Skill whose frontmatter name (and canonical
// name) is name, with the given body and (optional) bundle.
func demoSkill(t *testing.T, name, body string, bundle *spiceboxv1alpha1.SkillBundleRef) *spiceboxv1alpha1.Skill {
	t.Helper()
	return &spiceboxv1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: spiceboxv1alpha1.SkillSpec{
			CanonicalName: demoCanonical(name),
			Description:   "A demo skill for staging tests.",
			Body:          body,
			Frontmatter:   spiceboxv1alpha1.SkillFrontmatter{Name: name},
			Bundle:        bundle,
		},
	}
}

// stagingFixture bundles a session and the class that opts into staging
// against it — the two pieces every resolveAndStageSkillBundles call needs
// beyond the Skill CRs themselves.
type stagingFixture struct {
	sess  *spiceboxv1alpha1.AgentSession
	class *spiceboxv1alpha1.AgentClass
}

// demoSessionStaging opts a class into staging a skill under localName,
// pointed at the fixed "code-review" skill demoSkill(t, "code-review", ...)
// produces. When localName == "code-review" the local name matches the
// frontmatter name; any other value exercises the mismatch path.
func demoSessionStaging(t *testing.T, localName string) stagingFixture {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-review", Namespace: "ns", UID: "uid-stage"},
	}
	class := sandboxStagingClass(
		spiceboxv1alpha1.AgentSkill{Name: localName, Ref: demoCanonical("code-review"), Target: spiceboxv1alpha1.SkillTargetSandbox},
	)
	return stagingFixture{sess: sess, class: class}
}

// demoSessionWithAgentTargetedSkill opts a class into an AGENT-targeted skill
// (the default, load_skill-only), while its ToolBundle still names it via "*"
// in stageSkills. That is deliberate: the ONLY thing keeping it off disk must
// be the Target gate, not the absence of a stageSkills entry -- which is what
// makes this a real regression guard rather than a vacuous one.
func demoSessionWithAgentTargetedSkill(t *testing.T, localName string) stagingFixture {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-messaging", Namespace: "ns", UID: "uid-agent-target"},
	}
	class := sandboxStagingClass(
		spiceboxv1alpha1.AgentSkill{Name: localName, Ref: demoCanonical(localName), Target: spiceboxv1alpha1.SkillTargetAgent},
	)
	return stagingFixture{sess: sess, class: class}
}

// resolveFor runs resolveAndStageSkillBundles against fx with skills seeded
// into the fake client, returning the raw (resolved, err) so a test can
// inspect a returned error directly.
func resolveFor(t *testing.T, fx stagingFixture, skills ...*spiceboxv1alpha1.Skill) ([]spiceboxv1alpha1.ResolvedSkillBundle, error) {
	t.Helper()
	ctx := context.Background()
	sch := skillScheme(t)
	objs := make([]client.Object, 0, len(skills)+1)
	objs = append(objs, fx.sess)
	for _, sk := range skills {
		objs = append(objs, sk)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	r := &Reconciler{Client: c, BundleStore: skillbundlemem.New()}
	return r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
}

// stageFor is resolveFor plus fetching back each resolved entry's actual
// ConfigMap -- the independent observation the caller then untars, rather
// than asserting on the inputs used to build it.
func stageFor(t *testing.T, fx stagingFixture, skills ...*spiceboxv1alpha1.Skill) []*corev1.ConfigMap {
	t.Helper()
	ctx := context.Background()
	sch := skillScheme(t)
	objs := make([]client.Object, 0, len(skills)+1)
	objs = append(objs, fx.sess)
	for _, sk := range skills {
		objs = append(objs, sk)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	r := &Reconciler{Client: c, BundleStore: skillbundlemem.New()}
	resolved, err := r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
	require.NoError(t, err)

	cms := make([]*corev1.ConfigMap, 0, len(resolved))
	for _, rb := range resolved {
		var cm corev1.ConfigMap
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: fx.sess.Namespace, Name: rb.ConfigMapName}, &cm))
		cms = append(cms, &cm)
	}
	return cms
}

// untarConfigMap gunzips and untars a ConfigMap's bundle.tar.gz, returning its
// files by path. It reads the real bytes the ConfigMap carries -- never the
// inputs a test used to construct them -- so it is the independent
// observation TestResolveAndStageSkillBundles_* assertions rest on.
func untarConfigMap(t *testing.T, cm *corev1.ConfigMap) map[string][]byte {
	t.Helper()
	data := cm.BinaryData[skillBundleTarKey]
	require.NotEmpty(t, data, "ConfigMap must carry bundle.tar.gz bytes under the expected key")
	gz, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err, "staged bundle must be valid gzip")
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "staged bundle must be a valid tar archive")
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[hdr.Name] = content
	}
	return files
}

func TestResolveAndStageSkillBundles_InstructionOnlySkillStillStagesSKILLmd(t *testing.T) {
	// reviewbot's exact case: a methodology-only skill, spec.bundle == nil.
	skill := demoSkill(t, "code-review", "Review carefully.", nil)
	fx := demoSessionStaging(t, "code-review")

	cms := stageFor(t, fx, skill)

	require.Len(t, cms, 1, "an instruction-only skill must still stage when sandbox-targeted")
	files := untarConfigMap(t, cms[0])
	require.Contains(t, files, "SKILL.md",
		"the inner agent discovers a skill by reading SKILL.md; without it nothing is discoverable")
	assert.Contains(t, string(files["SKILL.md"]), "Review carefully.")
	assert.Contains(t, string(files["SKILL.md"]), "name: code-review",
		"frontmatter is re-serialized so the file is a valid skill, not a bare body")
}

func TestResolveAndStageSkillBundles_AgentTargetedSkillStagesNothing(t *testing.T) {
	skill := demoSkill(t, "messaging-fidelity", "Be faithful.", nil)
	fx := demoSessionWithAgentTargetedSkill(t, "messaging-fidelity")

	assert.Empty(t, stageFor(t, fx, skill),
		"an agent-targeted skill must produce no mount and no ConfigMap")
}

func TestResolveAndStageSkillBundles_SandboxNameMustMatchFrontmatter(t *testing.T) {
	skill := demoSkill(t, "code-review", "Review.", nil)
	fx := demoSessionStaging(t, "reviewer") // local name != frontmatter name

	_, err := resolveFor(t, fx, skill)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reviewer")
	assert.Contains(t, err.Error(), "code-review",
		"the error must name BOTH so the operator can see which to change")
}

// --- Fix round 1: SpiceboxMount.Digest must verify against the REAL bytes ---

// TestResolveAndStageSkillBundles_MountDigestMatchesActualArchiveBytes is the
// observable end-state check for the two-digest split: ResolvedSkillBundle.Digest
// is a cheap pre-fetch fingerprint (see sourceDigest in resolveAndStageSkillBundles)
// that can never equal the staged archive's real bytes hash, so the mount the
// sandbox pod actually verifies against must be fed from a SEPARATE field
// (ArchiveDigest). This asserts on bytes independently read back from the
// ConfigMap, not on anything production code handed the test.
func TestResolveAndStageSkillBundles_MountDigestMatchesActualArchiveBytes(t *testing.T) {
	ctx := context.Background()
	skill := demoSkill(t, "code-review", "Review carefully.", nil)
	fx := demoSessionStaging(t, "code-review")

	sch := skillScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(fx.sess, skill).Build()
	r := &Reconciler{Client: c, BundleStore: skillbundlemem.New()}

	resolved, err := r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
	require.NoError(t, err)
	require.Len(t, resolved, 1)

	// Independent observation: hash the ConfigMap's REAL bundle.tar.gz bytes,
	// read back from the fake API server, not from any value the production
	// code returned.
	var cm corev1.ConfigMap
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: fx.sess.Namespace, Name: resolved[0].ConfigMapName}, &cm))
	wantSum := sha256.Sum256(cm.BinaryData[skillBundleTarKey])
	wantDigest := hex.EncodeToString(wantSum[:])

	bundle := spiceboxv1alpha1.ToolBundle{Name: "demo-bundle", Class: "demo-class"}
	built := BuildBundleSession(fx.sess, bundle, "demo-identity", "", resolved, nil, nil)
	require.Len(t, built.Spec.Mounts, 1)
	assert.Equal(t, wantDigest, built.Spec.Mounts[0].Digest,
		"the mount's digest must match the ConfigMap's real bytes so the sandbox pod's sha256sum check verifies something real")
	assert.NotEqual(t, resolved[0].Digest, built.Spec.Mounts[0].Digest,
		"the mount digest must NOT be the cheap pre-fetch fingerprint -- that would guarantee a verification failure")
	// Fix round 1 (Task 10): the observable end-state this whole staging
	// feature exists for. resolved[0].LocalName is "code-review" (see
	// demoSessionStaging); the mount path Claude Code actually opens must be
	// exactly /skills/code-review, NOT /skills/<resolved[0].MountName>'s
	// content-hashed slug -- the frontmatter-name check above only means
	// anything if the directory it's checked against is the one that lands.
	assert.Equal(t, "code-review", resolved[0].LocalName)
	assert.Equal(t, "/skills/code-review", built.Spec.Mounts[0].MountPath,
		"Claude Code discovers a skill only when the sandbox directory name matches its SKILL.md frontmatter name")

	// The short-circuit path: re-run with status already carrying the first
	// pass's result. It must re-emit the SAME archive digest without rebuilding
	// the archive (no bytes were re-read to produce it).
	fx.sess.Status.ResolvedSkillBundles = resolved
	second, err := r.resolveAndStageSkillBundles(ctx, fx.sess, fx.class)
	require.NoError(t, err)
	assert.Equal(t, resolved, second,
		"a short-circuited pass must re-emit the identical resolved entry, ArchiveDigest included")

	builtAgain := BuildBundleSession(fx.sess, bundle, "demo-identity", "", second, nil, nil)
	require.Len(t, builtAgain.Spec.Mounts, 1)
	assert.Equal(t, wantDigest, builtAgain.Spec.Mounts[0].Digest,
		"the short-circuited mount must still verify against the real archive bytes, not a stale or empty value")
	assert.Equal(t, "/skills/code-review", builtAgain.Spec.Mounts[0].MountPath,
		"the short-circuited mount must still land at the local skill name, not regress to the hashed slug")
}
