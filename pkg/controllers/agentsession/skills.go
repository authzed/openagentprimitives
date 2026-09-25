// pkg/controllers/agentsession/skills.go
//
// Skill-bundle resolution + per-session ConfigMap materialization.
//
// The AgentSession reconciler resolves the AgentClass's opted-in skills
// (class.Spec.Skills, matched by AgentSkill.Ref) that some ToolBundle's
// StageSkills names, restricted to skills whose Target is "sandbox" or
// "both" (see skillsToStage). For each one it composes a SKILL.md from the
// resolved Skill/ClusterSkill's Body + Frontmatter + Description — re-adding
// exactly what discover.go's cached bundle archive deliberately omits — merges
// in any cached supporting files, and stages the result into a per-session
// ConfigMap (binaryData) owned by the session. The sandbox pod builder mounts
// those ConfigMaps and untars them into /skills, where a disk-based skill
// consumer (an inner Claude Code, or any tool that discovers skills by
// directory) can read them. A plain agent-targeted skill (the default) is
// never staged here at all — it reaches the agent exclusively through the
// load_skill tool, which reads Skill.Spec.Body directly and needs no mount.
//
// Resolution is best-effort and additive for per-skill misses: a missing
// Skill CR, a bundle the store hasn't cached, or an oversized composed
// archive is logged and skipped — never a session-boot failure (AGENTS.md:
// never silently drop errors). The one exception is a staged skill whose
// local AgentSkill.Name does not match its SKILL.md frontmatter name: Claude
// Code discovers a skill only when the directory name matches, so that
// mismatch is returned as a fatal, session-wide error naming both values —
// it is an authoring bug, not a transient miss, so retrying it silently
// would loop forever without ever fixing anything.
package agentsession

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

// skillBundleTarKey is the data key under which the gzipped-tar bundle is
// stored in the per-session ConfigMap. The pod builder reads this key.
const skillBundleTarKey = "bundle.tar.gz"

// configMapMaxBytes is the conservative ConfigMap size ceiling (~1MiB).
// Composed archives larger than this are skipped (large-bundle staging is
// deferred — spec §9); the skill still works instruction-only via load_skill.
const configMapMaxBytes = 1 << 20

// skillBundleConfigMapName is the deterministic per-session ConfigMap name for
// one staged bundle: "skillbundle-<session>-<mountName>". The mountName already
// carries a content-derived hash suffix (SafeSlug), so collisions across
// skills sharing a last segment are avoided.
func skillBundleConfigMapName(sessName, mountName string) string {
	return fmt.Sprintf("skillbundle-%s-%s", sessName, mountName)
}

// skillEntry holds the spec fields needed to compose a staged skill,
// extracted from either a namespace Skill or a cluster-scoped ClusterSkill.
type skillEntry struct {
	Description string
	Body        string
	Frontmatter spiceboxv1alpha1.SkillFrontmatter
	Bundle      *spiceboxv1alpha1.SkillBundleRef
}

// mergeSkillEntries builds a canonical-name → skillEntry map from the namespace
// and cluster sources. Namespace Skills take precedence: if both share a
// canonical name the namespace entry wins (same rule as the runner's
// mergeSkillSources). Pure function — no I/O.
func mergeSkillEntries(nsSkills []spiceboxv1alpha1.Skill, clusterSkills []spiceboxv1alpha1.ClusterSkill) map[string]skillEntry {
	merged := make(map[string]skillEntry, len(nsSkills)+len(clusterSkills))
	for i := range nsSkills {
		sk := &nsSkills[i]
		merged[sk.Spec.CanonicalName] = skillEntry{
			Description: sk.Spec.Description,
			Body:        sk.Spec.Body,
			Frontmatter: sk.Spec.Frontmatter,
			Bundle:      sk.Spec.Bundle,
		}
	}
	for i := range clusterSkills {
		csk := &clusterSkills[i]
		if _, present := merged[csk.Spec.CanonicalName]; !present {
			merged[csk.Spec.CanonicalName] = skillEntry{
				Description: csk.Spec.Description,
				Body:        csk.Spec.Body,
				Frontmatter: csk.Spec.Frontmatter,
				Bundle:      csk.Spec.Bundle,
			}
		}
	}
	return merged
}

// skillsToStage is the §4.4 expansion helper: it returns the set of
// AgentSkill.Name values that actually stage to disk for this class — a
// skill whose Target is "sandbox" or "both" AND that some ToolBundle's
// StageSkills names directly, or covers via "*" (meaning every
// sandbox/both-targeted skill). validateSkillsSpec (pkg/controllers/agentclass)
// checks the STRUCTURAL shape of StageSkills at class-admission time (names
// exist, target is not "agent"); this is the runtime counterpart that turns
// that shape into the concrete set to stage for one reconcile. Pure function
// over the class spec — no I/O.
func skillsToStage(spec spiceboxv1alpha1.AgentClassSpec) map[string]struct{} {
	sandboxNames := make(map[string]struct{})
	for _, sk := range spec.Skills {
		target := sk.Target
		if target == "" {
			// An in-memory AgentSkill has Target == "" until the API server
			// applies the CRD's +kubebuilder:default=agent — treat empty the
			// same as the explicit "agent" default (mirrors validateSkillsSpec).
			target = spiceboxv1alpha1.SkillTargetAgent
		}
		if target == spiceboxv1alpha1.SkillTargetSandbox || target == spiceboxv1alpha1.SkillTargetBoth {
			sandboxNames[sk.Name] = struct{}{}
		}
	}
	if len(sandboxNames) == 0 {
		return nil
	}

	stage := make(map[string]struct{})
	for _, b := range spec.ToolBundles {
		for _, staged := range b.StageSkills {
			if staged == "*" {
				for n := range sandboxNames {
					stage[n] = struct{}{}
				}
				continue
			}
			if _, ok := sandboxNames[staged]; ok {
				stage[staged] = struct{}{}
			}
			// A name that is not sandbox/both-targeted, or that names no
			// spec.skills entry at all, is silently ignored here: an
			// AgentClass reaching this reconciler is expected to already have
			// passed validateSkillsSpec, and this function stays additive/
			// non-fatal like the rest of this file rather than re-validating.
		}
	}
	return stage
}

// resolveAndStageSkillBundles resolves the AgentClass's sandbox-staged skills
// (see skillsToStage) to their Skill/ClusterSkill CRs, composes each one's
// SKILL.md + any cached supporting files into a tar.gz, and materializes it
// into a per-session ConfigMap. It returns the resolved-bundle snapshot the
// caller stamps into status (and the pod builder consumes). Per-skill misses
// are logged and skipped, never returned — one skill's problem must not fail
// the session. The one returned-error case is a local-name/frontmatter-name
// mismatch (§4.4 rule 4): that is a fatal, session-wide authoring bug, not a
// transient miss.
func (r *Reconciler) resolveAndStageSkillBundles(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	class *spiceboxv1alpha1.AgentClass,
) ([]spiceboxv1alpha1.ResolvedSkillBundle, error) {
	stageNames := skillsToStage(class.Spec)
	if len(stageNames) == 0 {
		return nil, nil
	}
	logger := log.FromContext(ctx)

	// List namespace Skills and index by canonical name (mirrors the runner's
	// resolveSkills). A List hiccup is additive-non-fatal: log + continue to
	// ClusterSkills so a namespace-List failure never suppresses cluster skills.
	var nsList spiceboxv1alpha1.SkillList
	if err := r.Client.List(ctx, &nsList, client.InNamespace(sess.Namespace)); err != nil {
		logger.Info("resolveAndStageSkillBundles: failed to list Skills; proceeding with cluster skills only",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		// nsList.Items remains nil — mergeSkillEntries handles that.
	}

	// List cluster-scoped ClusterSkills (no namespace). A List failure is
	// non-fatal: log + treat as no cluster skills so namespace skills still
	// work. Neither list failing aborts the other.
	var clusterList spiceboxv1alpha1.ClusterSkillList
	if err := r.Client.List(ctx, &clusterList); err != nil {
		logger.Info("resolveAndStageSkillBundles: failed to list ClusterSkills; proceeding without cluster skills",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		// clusterList.Items remains nil — mergeSkillEntries handles that.
	}

	byCanonical := mergeSkillEntries(nsList.Items, clusterList.Items)

	// What the LAST pass staged, indexed by canonical name. Staging is
	// re-entered on every reconcile, and the call site sits above a 2s requeue
	// with an 8-minute bundle-ready deadline, so a booting session reaches this
	// loop up to ~240 times.
	alreadyStaged := make(map[string]spiceboxv1alpha1.ResolvedSkillBundle, len(sess.Status.ResolvedSkillBundles))
	for _, rb := range sess.Status.ResolvedSkillBundles {
		alreadyStaged[rb.CanonicalName] = rb
	}

	resolved := make([]spiceboxv1alpha1.ResolvedSkillBundle, 0, len(stageNames))
	for _, sk := range class.Spec.Skills {
		if _, want := stageNames[sk.Name]; !want {
			continue
		}
		name := sk.Ref
		entry, ok := byCanonical[name]
		if !ok {
			// Opted-in for sandbox staging but not materialized yet —
			// non-fatal (matches the runner's agent-side resolution).
			logger.Info("resolveAndStageSkillBundles: skill opted-in for sandbox staging but not found; skipping",
				"session", sess.Namespace+"/"+sess.Name, "skill", name)
			continue
		}

		// §4.4 rule 4: a staged skill's local name must equal its SKILL.md
		// frontmatter name, because sk.Name (via ResolvedSkillBundle.LocalName,
		// below) becomes the actual directory name under /skills — see
		// BuildBundleSession — and a disk-based consumer (Claude Code)
		// discovers a skill only when those agree. validateSkillsSpec cannot
		// check this — it never resolves a Skill CR — so this is the one place
		// it can be enforced. Fatal (not log+skip): a mismatch never self-heals
		// by retrying, and naming both values is what lets an operator fix it.
		if entry.Frontmatter.Name != sk.Name {
			return nil, fmt.Errorf(
				"resolveAndStageSkillBundles: session %s: skill %q is staged to disk under local name %q, but its SKILL.md frontmatter name is %q — a disk-based consumer discovers a skill only when the directory name matches the frontmatter name; rename the AgentSkill or the SKILL.md frontmatter so they agree",
				sess.Namespace+"/"+sess.Name, name, sk.Name, entry.Frontmatter.Name,
			)
		}

		// MountName: SafeSlug is k8s-safe and collision-free across repos/
		// versions sharing a last segment. A canonical name that fails to parse
		// is non-fatal: log + skip rather than fail the whole session.
		parsed, perr := canonical.Parse(name)
		if perr != nil {
			logger.Info("resolveAndStageSkillBundles: skill canonical name failed to parse; skipping",
				"session", sess.Namespace+"/"+sess.Name, "skill", name, "err", perr.Error())
			continue
		}
		mountName := parsed.SafeSlug()
		cmName := skillBundleConfigMapName(sess.Name, mountName)

		skillMD, mdErr := renderSkillMD(entry)
		if mdErr != nil {
			logger.Info("resolveAndStageSkillBundles: failed to render SKILL.md; skipping",
				"session", sess.Namespace+"/"+sess.Name, "skill", name, "err", mdErr.Error())
			continue
		}

		// sourceDigest is a CHEAP, no-I/O fingerprint of everything that
		// determines the composed archive's bytes: the rendered SKILL.md (a
		// pure function of Description/Frontmatter/Body, already in memory
		// from the List above) plus the cached bundle's own digest, when
		// present. Because skillbundle.TarGz is a deterministic function of
		// its input files, an unchanged sourceDigest guarantees an unchanged
		// composed archive — which is what lets the check below skip the
		// store.Get entirely (up to 1 MiB from a live, uncached read) when
		// nothing has changed, exactly as the pre-composition version of this
		// function did, just widened to also react to Body/Frontmatter edits.
		mdSum := sha256.Sum256(skillMD)
		var bundleDigest string
		if entry.Bundle != nil {
			bundleDigest = entry.Bundle.Digest
		}
		srcSum := sha256.Sum256([]byte(hex.EncodeToString(mdSum[:]) + "|" + bundleDigest))
		sourceDigest := hex.EncodeToString(srcSum[:])

		// Already staged, unchanged: carry the status entry forward untouched.
		// Safe because the ConfigMap outlives the pod: it is owner-ref'd to the
		// AgentSession, and nothing but session finalization deletes it — a
		// wake-respawn re-mounts the same object. LocalName is included in the
		// comparison even though it never affects the ConfigMap's bytes: an
		// AgentClass author renaming sk.Name (with everything else unchanged)
		// must still produce a fresh ResolvedSkillBundle, because LocalName
		// alone drives BuildBundleSession's MountPath — carrying the OLD entry
		// forward would silently keep mounting under the stale directory name.
		if prev, staged := alreadyStaged[name]; staged &&
			prev.Digest == sourceDigest && prev.MountName == mountName &&
			prev.ConfigMapName == cmName && prev.LocalName == sk.Name {
			resolved = append(resolved, prev)
			continue
		}

		files := map[string][]byte{"SKILL.md": skillMD}

		if entry.Bundle != nil {
			data, err := r.BundleStore.Get(ctx, entry.Bundle.Digest)
			if err != nil {
				// Bundle not cached on this operator (or a store hiccup): the
				// skill still works instruction-only via load_skill. Log +
				// skip; never fail the session.
				if errors.Is(err, skillbundle.ErrNotFound) {
					logger.Info("resolveAndStageSkillBundles: bundle not in store; skipping (skill stays off-disk, still available via load_skill)",
						"session", sess.Namespace+"/"+sess.Name, "skill", name, "digest", entry.Bundle.Digest)
				} else {
					logger.Info("resolveAndStageSkillBundles: bundle store Get errored; skipping",
						"session", sess.Namespace+"/"+sess.Name, "skill", name, "digest", entry.Bundle.Digest, "err", err.Error())
				}
				continue
			}
			// Verify the bytes the store handed back actually hash to the
			// requested digest before mounting them into the sandbox. A
			// mismatch means the store is corrupt/poisoned; never stage
			// unverified bytes — log + skip so the skill stays off-disk
			// rather than executing tampered scripts.
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != entry.Bundle.Digest {
				logger.Info("resolveAndStageSkillBundles: staged bundle digest mismatch; skipping (skill stays off-disk)",
					"session", sess.Namespace+"/"+sess.Name, "skill", name, "digest", entry.Bundle.Digest, "computed", got)
				r.setFalseCondition(sess, spiceboxv1alpha1.AgentSessionConditionSkillBundlesIntegrity,
					spiceboxv1alpha1.ReasonAgentSessionSkillBundleDigestMismatch,
					fmt.Sprintf("skill %q bundle failed content-integrity check (store bytes hash %s, expected %s); not staged to disk",
						name, got, entry.Bundle.Digest))
				continue
			}
			bundleFiles, untarErr := untarGz(data)
			if untarErr != nil {
				logger.Info("resolveAndStageSkillBundles: cached bundle archive failed to unpack; skipping",
					"session", sess.Namespace+"/"+sess.Name, "skill", name, "err", untarErr.Error())
				continue
			}
			for p, b := range bundleFiles {
				if p == "SKILL.md" {
					// discover.go excludes SKILL.md from the cached archive
					// already; this guard is defense-in-depth against a
					// hand-authored bundle that carries one anyway, so it
					// never clobbers the freshly re-serialized SKILL.md.
					continue
				}
				files[p] = b
			}
		}

		// archiveDigest is the sha256 of the ACTUAL archive bytes about to be
		// written to the ConfigMap -- skillbundle.TarGz already computes it as
		// part of building the archive, so no second pass over the bytes is
		// needed. This is what SpiceboxMount.Digest verifies against in the
		// sandbox pod's init container; it is deliberately distinct from
		// sourceDigest above, which is a cheap pre-fetch fingerprint and does
		// not equal the hash of these bytes.
		archive, archiveDigest, tarErr := skillbundle.TarGz(files)
		if tarErr != nil {
			logger.Info("resolveAndStageSkillBundles: failed to build the composed skill archive; skipping",
				"session", sess.Namespace+"/"+sess.Name, "skill", name, "err", tarErr.Error())
			continue
		}
		if len(archive) > configMapMaxBytes {
			// Large-bundle staging is deferred (spec §9): a ConfigMap cannot
			// hold it. Log + skip; the skill stays off-disk.
			logger.Info("resolveAndStageSkillBundles: composed skill bundle exceeds ConfigMap ceiling; skipping (large-bundle staging deferred)",
				"session", sess.Namespace+"/"+sess.Name, "skill", name,
				"bytes", len(archive), "ceiling", configMapMaxBytes)
			continue
		}

		if err := r.writeSkillBundleConfigMap(ctx, sess, cmName, archive); err != nil {
			// A ConfigMap write failure for one bundle is additive-non-fatal:
			// log + skip so the rest of the session (and other bundles) proceed.
			logger.Info("resolveAndStageSkillBundles: failed to write bundle ConfigMap; skipping",
				"session", sess.Namespace+"/"+sess.Name, "skill", name, "configMap", cmName, "err", err.Error())
			continue
		}

		resolved = append(resolved, spiceboxv1alpha1.ResolvedSkillBundle{
			CanonicalName: name,
			LocalName:     sk.Name,
			MountName:     mountName,
			ConfigMapName: cmName,
			Digest:        sourceDigest,
			ArchiveDigest: archiveDigest,
		})
	}
	return resolved, nil
}

// skillFrontmatterDoc is the on-disk YAML shape of a SKILL.md's frontmatter
// block. sigs.k8s.io/yaml.Marshal round-trips through an untyped
// map[string]interface{} on its way to YAML, so the emitted keys land in
// alphabetical order (allowedTools, compatibility, description, license,
// metadata, name), not struct declaration order — that's fine here, since a
// SKILL.md parser reads frontmatter by key, not position. Marshaling this
// type rather than hand-formatting is still what matters: it gets quoting
// and escaping right on values a hand-built YAML block would get wrong.
type skillFrontmatterDoc struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	AllowedTools  string            `json:"allowedTools,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// renderSkillMD re-serializes a resolved skill's spec fields into the SKILL.md
// this composes into every staged skill bundle. Skill.Spec.Body already holds
// the markdown body; discover.go deliberately excludes SKILL.md from a
// bundle's cached archive because the agent gets it via load_skill instead —
// staging re-assembles the whole file for the disk-based consumer (an inner
// Claude Code, or any tool that discovers skills by directory), which
// load_skill never serves.
func renderSkillMD(entry skillEntry) ([]byte, error) {
	fm := skillFrontmatterDoc{
		Name:          entry.Frontmatter.Name,
		Description:   entry.Description,
		License:       entry.Frontmatter.License,
		Compatibility: entry.Frontmatter.Compatibility,
		AllowedTools:  entry.Frontmatter.AllowedTools,
		Metadata:      entry.Frontmatter.Metadata,
	}
	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("marshal SKILL.md frontmatter: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(bytes.TrimRight(fmBytes, "\n"))
	buf.WriteString("\n---\n")
	buf.WriteString(entry.Body)
	return buf.Bytes(), nil
}

// untarGz reverses skillbundle.TarGz: it reads a cached bundle's gzipped-tar
// archive back into a path→content map, so its supporting files can be
// re-composed alongside a freshly rendered SKILL.md into one staged archive.
// discover.go is the only writer of these archives, so regular files are all
// this ever needs to handle.
func untarGz(data []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
		}
		files[hdr.Name] = content
	}
	return files, nil
}

// writeSkillBundleConfigMap create-or-updates a per-session ConfigMap owned by
// the AgentSession (cascade-deleted on session finalization via owner-ref GC).
// Idempotent: Create, and on AlreadyExists, Get + Update BinaryData so a
// re-reconcile with a re-resolved bundle rewrites the value. Mirrors
// writeSidecarSecret.
func (r *Reconciler) writeSkillBundleConfigMap(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, name string, data []byte) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       sess.Namespace,
			OwnerReferences: sessionOwnerRef(sess),
		},
		BinaryData: map[string][]byte{skillBundleTarKey: data},
	}
	// Stamp the adoption label so this operator-minted ConfigMap enters the
	// label-filtered cache and passes the ConfigMapReader guard on subsequent reads.
	adoptguard.WithAdoptedLabel(cm)
	if err := r.Client.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create skill-bundle configmap %q: %w", name, err)
		}
		existing, getErr := r.getConfigMap(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: name})
		if getErr != nil {
			return fmt.Errorf("get existing skill-bundle configmap %q: %w", name, getErr)
		}
		existing.BinaryData = map[string][]byte{skillBundleTarKey: data}
		if updErr := r.Client.Update(ctx, existing); updErr != nil {
			return fmt.Errorf("update skill-bundle configmap %q: %w", name, updErr)
		}
	}
	return nil
}
