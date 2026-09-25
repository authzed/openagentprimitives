// pkg/controllers/clusterskillsource/controller.go
//
// Package clusterskillsource reconciles ClusterSkillSource objects: the
// cluster-scoped mirror of pkg/controllers/skillsource. It resolves the
// (optional) clone credential from a directly-referenced Secret, fetches the
// repo at the requested ref, discovers SKILL.md files, caches each skill's
// bundle by content digest, and materializes owned cluster-scoped ClusterSkill
// CRs.
//
// It differs from the namespaced SkillSource controller in exactly three ways:
//
//  1. The object is cluster-scoped (no namespace on the ClusterSkillSource).
//  2. Auth is a direct SecretRef resolved by reading the Secret in
//     spec.Auth.Namespace — AgentIdentity is namespaced and unavailable at
//     cluster scope, so there is no credresolve/AgentIdentity indirection.
//  3. The materialized children are cluster-scoped ClusterSkills (no namespace
//     on the child), owner-ref'd to the ClusterSkillSource.
//
// Everything else — fetch behind skillfetch.Fetcher, the bundle cache behind
// skillbundle.Store, discovery via the scope-agnostic skillsource.Discover, the
// owner-ref get-or-create-or-update idiom, and the failure handling (surface on
// the Ready condition + log, NOT return, so a bad credential/ref requeues on the
// sync interval instead of crash-looping) — mirrors the namespaced controller.
//
// The scope-agnostic halves of a sync pass (credential extraction, the bundle
// cache write, the desired-spec mapping, the status record) are CALLED from
// skillsource, not re-implemented here: keeping copies is what let the empty
// auth-value check drift out of this file once already.
package clusterskillsource

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillspec"
	"github.com/authzed/openagentprimitives/pkg/controllers/skillsource"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/skillfetch"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// defaultSyncInterval is the re-poll cadence when neither the controller nor the
// ClusterSkillSource spec sets one.
const defaultSyncInterval = time.Hour

// failureRetryInterval is the re-poll cadence after a failed pass. A failure is
// usually something an operator is actively repairing — a bad credential, a
// moved ref, an unreachable host — so the wait for the repair to be noticed is
// the wait that matters, not the poll budget the success path is tuned for. The
// Secret watch in watch.go carries a credential fix through immediately; this
// bounds every failure it cannot see to minutes instead of the full sync
// interval. Kept in step with the namespaced controller's constant.
const failureRetryInterval = 2 * time.Minute

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskillsources,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskillsources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconciler fetches a ClusterSkillSource's repo, caches skill bundles, and
// materializes owned ClusterSkill CRs.
type Reconciler struct {
	Client      client.Client
	BundleStore skillbundle.Store
	Fetcher     skillfetch.Fetcher
	// SyncInterval overrides the default re-poll cadence (1h). A
	// ClusterSkillSource's spec.sync.interval, when set, takes precedence over
	// both.
	SyncInterval time.Duration
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src v1.ClusterSkillSource
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &src); !cont {
		return ctrl.Result{}, err
	}
	if src.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	interval := r.interval(&src)

	// Step 2: resolve the clone credential (empty token when no auth is set).
	token, err := r.resolveToken(ctx, &src)
	if err != nil {
		logger.Info("ClusterSkillSource auth resolution failed", "clusterskillsource", src.Name,
			"err", err.Error())
		conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
			v1.ReasonSkillSourceAuthResolveFailed, err.Error())
		return r.requeue(ctx, &src, interval)
	}

	// Step 3: fetch the repo at the requested ref.
	result, err := r.Fetcher.Fetch(ctx, skillfetch.Request{
		RepoURL: src.Spec.RepoURL,
		Ref:     src.Spec.Ref,
		Token:   token,
	})
	if err != nil {
		logger.Info("ClusterSkillSource fetch failed", "clusterskillsource", src.Name,
			"repoURL", src.Spec.RepoURL, "err", err.Error())
		conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
			v1.ReasonSkillSourceFetchFailed, err.Error())
		return r.requeue(ctx, &src, interval)
	}

	// Step 4: discover skills from the fetched tree (problems are advisory, not fatal).
	discovered, repoInstrDiscovered, problems := skillsource.Discover(
		canonical.Normalize(src.Spec.RepoURL), src.Spec.Subpath, src.Spec.Ref, result.Files)
	for _, p := range problems {
		logger.Info("ClusterSkillSource discovery problem", "clusterskillsource", src.Name,
			"problem", p)
	}

	// Step 4b: compute opt-out-aware capped repo instructions once for all skills.
	var repoInstr *v1.SkillRepoInstructions
	if !src.Spec.DisableRepoInstructions {
		var warning string
		repoInstr, warning = skillsource.BuildRepoInstructions(repoInstrDiscovered)
		if warning != "" {
			problems = append(problems, warning)
		}
	}

	// Step 5: cache each bundle + upsert the owned ClusterSkill.
	//
	// Runs on EVERY pass — CR status is not evidence that the owned objects
	// still exist. See the same step in the namespaced controller for the two
	// failure modes an unchanged-status skip made permanent (an operator-restart
	// -emptied bundle store, and a deleted child).
	materialized := false
	for _, d := range discovered {
		wrote, err := skillsource.EnsureBundle(ctx, r.BundleStore, d)
		if err != nil {
			logger.Info("ClusterSkillSource bundle cache failed", "clusterskillsource", src.Name,
				"canonicalName", d.CanonicalName, "err", err.Error())
			conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
				v1.ReasonSkillSourceMaterializeFailed, err.Error())
			return r.requeue(ctx, &src, interval)
		}
		materialized = materialized || wrote

		upserted, err := r.upsertClusterSkill(ctx, &src, d, repoInstr, result.SHA)
		if err != nil {
			logger.Info("ClusterSkillSource materialize failed", "clusterskillsource", src.Name,
				"canonicalName", d.CanonicalName, "err", err.Error())
			conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
				v1.ReasonSkillSourceMaterializeFailed,
				fmt.Sprintf("materializing %s: %v", d.CanonicalName, err))
			return r.requeue(ctx, &src, interval)
		}
		materialized = materialized || upserted
	}

	// Step 6: record success on status, writing only when something moved (see
	// skillsource.RecordSync — an unconditional write would re-enqueue this
	// reconcile, and its git fetch, forever).
	if skillsource.RecordSync(&src, &src.Status, skillsource.SyncResult{
		ResolvedSHA:      result.SHA,
		DiscoveredSkills: len(discovered),
		Problems:         problems,
		Materialized:     materialized,
	}) {
		if err := r.Client.Status().Update(ctx, &src); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// resolveToken resolves the clone credential to a token string, or "" when the
// ClusterSkillSource has no auth configured. Unlike the namespaced SkillSource
// (which dereferences an AgentIdentity credential), the cluster source reads the
// referenced Secret directly in spec.Auth.Namespace — AgentIdentity is
// namespaced and cannot be referenced from cluster scope. A missing Secret, or
// a key that is absent or empty, is an error the caller surfaces on the Ready
// condition; the emptiness check lives in skillsource.TokenFromSecret so it
// cannot drift away from the namespaced controller again.
func (r *Reconciler) resolveToken(ctx context.Context, src *v1.ClusterSkillSource) (string, error) {
	if src.Spec.Auth == nil {
		return "", nil
	}
	auth := src.Spec.Auth

	secretRef := types.NamespacedName{Namespace: auth.Namespace, Name: auth.SecretRef.Name}
	ownerRef := types.NamespacedName{Name: src.Name} // cluster-scoped; no namespace

	// Adopt the referenced Secret before reading it — metadata-only SSA that
	// stamps AdoptedLabel so subsequent reads via SecretReader are permitted.
	// The existence check inside Adopt uses the live reader (r.SecretReader.Reader)
	// so a not-yet-adopted Secret (absent from the label-filtered cache) is seen.
	if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "ClusterSkillSource"); err != nil {
		return "", fmt.Errorf("adopt Secret %s/%s: %w", auth.Namespace, auth.SecretRef.Name, err)
	}

	sec, err := r.SecretReader.Get(ctx, secretRef)
	if err != nil {
		return "", fmt.Errorf("get Secret %s/%s: %w", auth.Namespace, auth.SecretRef.Name, err)
	}
	return skillsource.TokenFromSecret(sec, secretRef, auth.SecretRef.Key)
}

// upsertClusterSkill creates or updates the owned ClusterSkill CR for a
// discovered skill and reports whether it wrote. The metadata.name is the
// canonical name's SafeSlug; the ClusterSkillSource is set as the controller
// owner so k8s GC prunes it on source deletion. The ClusterSkill is
// cluster-scoped, so the child carries no namespace. Mirrors the
// get-or-create-or-update idiom (incl. owner-ref adoption) in the namespaced
// SkillSource controller.
func (r *Reconciler) upsertClusterSkill(
	ctx context.Context, src *v1.ClusterSkillSource, d skillsource.DiscoveredSkill,
	repoInstr *v1.SkillRepoInstructions, resolvedSHA string,
) (bool, error) {
	name, err := canonical.Parse(d.CanonicalName)
	if err != nil {
		return false, fmt.Errorf("parse canonical name %q: %w", d.CanonicalName, err)
	}
	slug := name.SafeSlug()

	spec := skillsource.DesiredSkillSpec(
		skillsource.SourceRef{RepoURL: src.Spec.RepoURL, Ref: src.Spec.Ref, Name: src.Name},
		d, repoInstr, resolvedSHA)

	desired := &v1.ClusterSkill{
		ObjectMeta: metav1.ObjectMeta{
			Name: slug,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1.SchemeGroupVersion.String(),
				Kind:               "ClusterSkillSource",
				Name:               src.Name,
				UID:                src.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spec,
	}

	var existing v1.ClusterSkill
	err = r.Client.Get(ctx, client.ObjectKey{Name: slug}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create ClusterSkill/%s: %w", slug, err)
		}
	case err != nil:
		return false, fmt.Errorf("get ClusterSkill/%s: %w", slug, err)
	default:
		// Reconcile the owner reference: a pre-existing ClusterSkill with no/wrong
		// owner ref must be adopted so GC and self-heal work correctly.
		wantOwnerRef := desired.OwnerReferences[0]
		if skillspec.Equal(existing.Spec, spec) && skillspec.OwnerRefPresent(existing.OwnerReferences, wantOwnerRef) {
			return false, nil // no change
		}
		cp := existing.DeepCopy()
		cp.Spec = spec
		cp.OwnerReferences = skillspec.AdoptOwnerRef(cp.OwnerReferences, wantOwnerRef)
		if err := r.Client.Update(ctx, cp); err != nil {
			return false, fmt.Errorf("update ClusterSkill/%s: %w", slug, err)
		}
	}
	return true, nil
}

// requeue writes status and returns a requeue-after-interval result with no
// error (failures are condition-surfaced, not returned — see package doc).
// requeue records the failure on status and schedules the retry. Every caller is
// a failure path, so the cadence is clamped to failureRetryInterval here rather
// than at each call site — a future failure path cannot forget it. The clamp is a
// floor on frequency, never a ceiling: a source asking to be polled faster than
// the failure cadence keeps its own interval.
func (r *Reconciler) requeue(ctx context.Context, src *v1.ClusterSkillSource, interval time.Duration) (ctrl.Result, error) {
	if err := r.Client.Status().Update(ctx, src); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: min(interval, failureRetryInterval)}, nil
}

// interval picks the effective re-poll cadence: spec.sync.interval, else the
// reconciler's SyncInterval, else the 1h default.
func (r *Reconciler) interval(src *v1.ClusterSkillSource) time.Duration {
	if d := src.Spec.Sync.Interval.Duration; d > 0 {
		return d
	}
	if r.SyncInterval > 0 {
		return r.SyncInterval
	}
	return defaultSyncInterval
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ClusterSkillSource{}).
		Owns(&v1.ClusterSkill{}).
		// Without this a rotated PAT is invisible until the resync. See watch.go.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToSources)).
		Complete(r)
}
