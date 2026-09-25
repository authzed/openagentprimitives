// pkg/controllers/skillsource/sync.go
//
// The scope-agnostic pieces of one sync pass, shared with the cluster-scoped
// mirror in pkg/controllers/clusterskillsource. `ClusterSkillSource.Status` IS
// `v1.SkillSourceStatus` and both controllers materialize `v1.SkillSpec`, so
// the credential extraction, the bundle-cache write, the desired-spec mapping
// and the status record live here once. They used to be byte-identical copies
// in the two controllers, and had already drifted user-visibly: the cluster
// copy accepted an empty auth value the namespaced copy rejected, silently
// downgrading a credentialed clone to an anonymous one.
package skillsource

import (
	"context"
	"fmt"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillspec"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TokenFromSecret reads key out of sec and returns it as a clone token.
//
// A key that is absent OR present-but-empty is an error, never a silent "".
// skillfetch.Request documents an empty Token as "clone anonymously", so
// returning "" for an empty Secret value downgrades a credentialed clone to an
// anonymous one: Ready then reports FetchFailed (private repo) or — worse —
// Synced (public repo) instead of AuthResolveFailed, and the operator has no
// signal that the credential they opted into was never used.
func TokenFromSecret(sec *corev1.Secret, ref types.NamespacedName, key string) (string, error) {
	val, ok := sec.Data[key]
	if !ok || len(val) == 0 {
		return "", fmt.Errorf("%w: %s/%s key=%q",
			credresolve.ErrSecretKeyMissing, ref.Namespace, ref.Name, key)
	}
	return string(val), nil
}

// EnsureBundle caches d's bundle when the store does not already hold it, and
// reports whether it wrote. Instruction-only skills carry no bundle and are a
// no-op.
//
// The Has-before-Put is what keeps a steady-state pass write-free. Both
// controllers run materialization on EVERY reconcile — CR status is not
// evidence that the store still holds anything, since the in-memory backend
// (which every non-postgres install gets, sqlite included) empties on operator
// restart — so the common case must cost a lookup, not a write.
func EnsureBundle(ctx context.Context, store skillbundle.Store, d DiscoveredSkill) (bool, error) {
	if d.BundleDigest == "" {
		return false, nil
	}
	present, err := store.Has(ctx, d.BundleDigest)
	if err != nil {
		return false, fmt.Errorf("checking bundle cache for %s: %w", d.CanonicalName, err)
	}
	if present {
		return false, nil
	}
	if err := store.Put(ctx, d.BundleDigest, d.BundleTarGz); err != nil {
		return false, fmt.Errorf("caching bundle for %s: %w", d.CanonicalName, err)
	}
	return true, nil
}

// SourceRef is the identifying slice of a (Cluster)SkillSource spec that lands
// on every skill it materializes.
type SourceRef struct {
	// RepoURL is the raw spec.repoURL (normalized here, not by the caller).
	RepoURL string
	// Ref is spec.ref — the branch/tag/SHA requested.
	Ref string
	// Name is the source CR's metadata.name.
	Name string
}

// DesiredSkillSpec maps a DiscoveredSkill (+ repo instructions + resolved SHA)
// onto the SkillSpec both Skill and ClusterSkill carry.
func DesiredSkillSpec(
	src SourceRef, d DiscoveredSkill,
	repoInstr *v1.SkillRepoInstructions, resolvedSHA string,
) v1.SkillSpec {
	spec := v1.SkillSpec{
		CanonicalName:    d.CanonicalName,
		DisplayName:      d.Frontmatter.Name,
		Description:      d.Description,
		Body:             d.Body,
		Frontmatter:      skillspec.ToV1Frontmatter(d.Frontmatter),
		RepoInstructions: repoInstr,
		Source: &v1.SkillProvenance{
			RepoLocator: canonical.Normalize(src.RepoURL),
			Subpath:     d.Subpath,
			Ref:         src.Ref,
			ResolvedSHA: resolvedSHA,
			SourceName:  src.Name,
		},
	}
	if d.BundleDigest != "" {
		spec.Bundle = &v1.SkillBundleRef{Digest: d.BundleDigest, CacheKey: d.BundleDigest}
	}
	return spec
}

// SyncResult is what one completed materialization pass observed.
type SyncResult struct {
	// ResolvedSHA is the commit the fetch resolved to ("" when the fetcher
	// reports none).
	ResolvedSHA string
	// DiscoveredSkills is the number of skills materialized this pass.
	DiscoveredSkills int
	// Problems are the advisory discovery problems, in deterministic order
	// (Discover sorts them; any caller-appended warning goes last).
	Problems []string
	// Materialized reports whether the pass actually wrote something — a bundle
	// into the cache, or a child Skill/ClusterSkill.
	Materialized bool
}

// RecordSync writes a completed pass's outcome onto st and reports whether the
// status moved. A caller MUST skip its Status().Update when it returns false.
//
// Every field except lastSyncTime is a pure function of the fetched tree and
// the object's generation, so a pass that changed nothing recomputes exactly
// the bytes already stored. lastSyncTime is the one volatile value and is
// stamped only when the pass materialized something or the derived fields
// actually moved (repo idiom: observations in controller-owned status, set on
// meaningful change).
//
// That rule is load-bearing, not hygiene. Both controllers watch their own
// object with no predicate and re-materialize on every pass, so an
// unconditional re-stamp would make each pass re-enqueue itself — with a git
// fetch per iteration — forever.
func RecordSync(obj conditions.Generationer, st *v1.SkillSourceStatus, res SyncResult) bool {
	prev := st.DeepCopy()

	st.ResolvedSHA = res.ResolvedSHA
	st.DiscoveredSkills = int32(res.DiscoveredSkills)
	st.ObservedGeneration = obj.GetGeneration()
	st.DiscoveryProblems = CapProblems(res.Problems)
	switch {
	case res.DiscoveredSkills == 0:
		// A pass that materialized nothing has produced nothing for any
		// AgentClass to opt into, so Ready is False even though the clone
		// itself succeeded. Ready is the one signal read without asking for
		// detail — the READY column of `oap skill source list`, the headline of
		// `kubectl get skillsource` — and reporting Synced here renders a source
		// that yielded nothing identically to one that yielded everything,
		// leaving a parked AgentClass as the only thing anywhere that looks
		// wrong. This is also what SkillSourceConditionReady already documents:
		// True when the last sync "cloned, discovered, cached, and materialized
		// Skills".
		msg := "fetched, but no skill materialized: the fetched tree contains no SKILL.md"
		if len(res.Problems) > 0 {
			msg = fmt.Sprintf(
				"fetched, but no skill materialized; %d discovery problem(s) — see status.discoveryProblems",
				len(res.Problems))
		}
		conditions.Set(obj, &st.Conditions, metav1.Condition{
			Type:    v1.SkillSourceConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  v1.ReasonSkillSourceNoSkillsDiscovered,
			Message: msg,
		})
	case len(res.Problems) == 0:
		conditions.SetTrue(obj, &st.Conditions, v1.SkillSourceConditionReady,
			v1.ReasonSkillSourceSynced)
	default:
		conditions.Set(obj, &st.Conditions, metav1.Condition{
			Type:   v1.SkillSourceConditionReady,
			Status: metav1.ConditionTrue,
			Reason: v1.ReasonSkillSourceSynced,
			Message: fmt.Sprintf(
				"synced; %d SKILL.md file(s) skipped as invalid — see status.discoveryProblems",
				len(res.Problems)),
		})
	}

	// lastSyncTime is identical in prev and st here, so this compares only the
	// derived fields.
	moved := res.Materialized || !equality.Semantic.DeepEqual(*prev, *st)
	if moved {
		now := metav1.Now()
		st.LastSyncTime = &now
	}
	return moved
}
