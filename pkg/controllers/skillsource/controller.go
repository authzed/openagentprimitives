// pkg/controllers/skillsource/controller.go
//
// Package skillsource reconciles SkillSource objects: it resolves the (optional)
// clone credential, fetches the repo at the requested ref, discovers SKILL.md
// files, caches each skill's bundle by content digest, and materializes owned
// Skill CRs. Fetch is abstracted behind skillfetch.Fetcher (go-git in prod, a
// fake in tests); the bundle cache behind skillbundle.Store. Failures surface on
// the Ready condition and are logged with context — they are NOT returned, so a
// bad credential/ref doesn't crash-loop the reconcile (it requeues on the sync
// interval). k8s owner-ref GC prunes Skills when the whole SkillSource is
// deleted; per-skill orphan pruning is a follow-up (see Plan 2 deferred list).
//
// Materialization is level-triggered: every pass re-caches the bundles and
// re-upserts the Skills, because CR status is not evidence that either still
// exists. Both writes are idempotent, and the status write is skipped entirely
// when nothing moved — see RecordSync in sync.go for why that is what keeps the
// self-watch quiet.
package skillsource

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillspec"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credhost"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
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
// SkillSource spec sets one.
const defaultSyncInterval = time.Hour

// failureRetryInterval is the re-poll cadence after a failed pass. A failure is
// usually something an operator is actively repairing — a bad credential, a
// moved ref, an unreachable host — so the wait for the repair to be noticed is
// the wait that matters, not the poll budget the success path is tuned for. The
// dependency watches in watch.go carry a credential fix through immediately;
// this bounds every failure they cannot see (a ref that starts resolving, a host
// that comes back) to minutes instead of the full sync interval.
const failureRetryInterval = 2 * time.Minute

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skillsources,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skillsources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skills,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconciler fetches a SkillSource's repo, caches skill bundles, and
// materializes owned Skill CRs.
type Reconciler struct {
	Client      client.Client
	BundleStore skillbundle.Store
	Fetcher     skillfetch.Fetcher
	// SyncInterval overrides the default re-poll cadence (1h). A SkillSource's
	// spec.sync.interval, when set, takes precedence over both.
	SyncInterval time.Duration
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var src v1.SkillSource
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
		logger.Info("SkillSource auth resolution failed", "skillsource", src.Name,
			"namespace", src.Namespace, "err", err.Error())
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
		logger.Info("SkillSource fetch failed", "skillsource", src.Name,
			"namespace", src.Namespace, "repoURL", src.Spec.RepoURL, "err", err.Error())
		conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
			v1.ReasonSkillSourceFetchFailed, err.Error())
		return r.requeue(ctx, &src, interval)
	}

	// Step 4: discover skills from the fetched tree (problems are advisory, not fatal).
	discovered, repoInstrDiscovered, problems := Discover(
		canonical.Normalize(src.Spec.RepoURL), src.Spec.Subpath, src.Spec.Ref, result.Files)
	for _, p := range problems {
		logger.Info("SkillSource discovery problem", "skillsource", src.Name,
			"namespace", src.Namespace, "problem", p)
	}

	// Step 4b: compute opt-out-aware capped repo instructions once for all skills.
	var repoInstr *v1.SkillRepoInstructions
	if !src.Spec.DisableRepoInstructions {
		var warning string
		repoInstr, warning = BuildRepoInstructions(repoInstrDiscovered)
		if warning != "" {
			problems = append(problems, warning)
		}
	}

	// Step 5: cache each bundle + upsert the owned Skill.
	//
	// This runs on EVERY pass, not only when the resolved SHA or the generation
	// moved. CR status is not evidence that the owned objects still exist: the
	// bundle store is process-local for every non-postgres install (sqlite —
	// `oap init --local` / `oap desktop` — included) and comes back empty after an
	// operator restart, and a Skill can be deleted out from under us. Skipping
	// on unchanged status made both permanent: the re-Put never happened and the
	// deleted child was never recreated, while status kept reporting
	// Ready/Synced. Both writes are idempotent (Has-gated content-addressed Put,
	// spec-equal upsert); `materialized` records whether this pass wrote
	// anything so the status write below stays byte-identical when it did not.
	materialized := false
	for _, d := range discovered {
		wrote, err := EnsureBundle(ctx, r.BundleStore, d)
		if err != nil {
			logger.Info("SkillSource bundle cache failed", "skillsource", src.Name,
				"namespace", src.Namespace, "canonicalName", d.CanonicalName, "err", err.Error())
			conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
				v1.ReasonSkillSourceMaterializeFailed, err.Error())
			return r.requeue(ctx, &src, interval)
		}
		materialized = materialized || wrote

		upserted, err := r.upsertSkill(ctx, &src, d, repoInstr, result.SHA)
		if err != nil {
			logger.Info("SkillSource materialize failed", "skillsource", src.Name,
				"namespace", src.Namespace, "canonicalName", d.CanonicalName, "err", err.Error())
			conditions.SetFalse(&src, &src.Status.Conditions, v1.SkillSourceConditionReady,
				v1.ReasonSkillSourceMaterializeFailed,
				fmt.Sprintf("materializing %s: %v", d.CanonicalName, err))
			return r.requeue(ctx, &src, interval)
		}
		materialized = materialized || upserted
	}

	// Step 6: record success on status, writing only when something moved (see
	// RecordSync — an unconditional write would re-enqueue this reconcile, and
	// its git fetch, forever).
	if RecordSync(&src, &src.Status, SyncResult{
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

// CapProblems bounds the status list so it can't grow unbounded; excess is
// summarized (the full set is always in the logs). Exported so the
// clusterskillsource controller can reuse it without duplication.
const maxStatusProblems = 10

func CapProblems(problems []string) []string {
	if len(problems) <= maxStatusProblems {
		return problems
	}
	out := append([]string{}, problems[:maxStatusProblems]...)
	return append(out, fmt.Sprintf("…and %d more (see logs)", len(problems)-maxStatusProblems))
}

// resolveToken resolves the clone credential to a token string, or "" when the
// SkillSource has no auth configured. A nil/missing credential or secret, or a
// key that is absent or empty, is an error the caller surfaces on the Ready
// condition — see credresolve.ResolveSecretValue for why an empty value is not
// "anonymous".
func (r *Reconciler) resolveToken(ctx context.Context, src *v1.SkillSource) (string, error) {
	if src.Spec.Auth == nil {
		return "", nil
	}
	auth := src.Spec.Auth

	var id v1.AgentIdentity
	key := client.ObjectKey{Namespace: src.Namespace, Name: auth.AgentIdentity}
	if err := r.Client.Get(ctx, key, &id); err != nil {
		return "", fmt.Errorf("get AgentIdentity %s/%s: %w", src.Namespace, auth.AgentIdentity, err)
	}

	var cred *v1.AgentCredential
	for i := range id.Spec.Credentials {
		if id.Spec.Credentials[i].Name == auth.Credential {
			cred = &id.Spec.Credentials[i]
			break
		}
	}
	if cred == nil {
		return "", fmt.Errorf("AgentIdentity %s/%s has no credential %q",
			src.Namespace, auth.AgentIdentity, auth.Credential)
	}

	// WHERE the credential may go, before resolving WHAT it is.
	//
	// spec.repoURL and spec.auth are two independent fields on the same
	// tenant-writable object, and nothing tied them together: the operator
	// resolved any credential on any AgentIdentity in the namespace and handed
	// the raw value to the clone as an HTTP Basic password against whatever
	// host the same tenant wrote. It reads the Secret with its own cluster-wide
	// credentials, so the actor never needed `get secrets`.
	//
	// An UNSCOPED credential is refused here rather than allowed. credhost.Check
	// has to treat empty allowedHosts as permissive — anything else breaks every
	// existing credential on upgrade — but there is no safe default destination
	// for a git clone, so this path demands the scope instead of inheriting that
	// default. Checked before the Secret is adopted or read, so a refusal never
	// touches the value.
	if len(cred.AllowedHosts) == 0 {
		return "", fmt.Errorf("skillsource %s/%s: credential %q on AgentIdentity %q declares no allowedHosts, "+
			"so it may not be sent to spec.repoURL. Set spec.credentials[].allowedHosts on that credential "+
			"to the git host(s) it belongs to (e.g. [\"github.com\"])",
			src.Namespace, src.Name, cred.Name, auth.AgentIdentity)
	}
	if err := credhost.Check(*cred, src.Spec.RepoURL); err != nil {
		return "", fmt.Errorf("skillsource %s/%s: %w", src.Namespace, src.Name, err)
	}

	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return "", fmt.Errorf("skillsource %s/%s credential %q: %w", src.Namespace, src.Name, cred.Name, err)
	}
	ref := k.SecretRef(*cred)
	if ref == nil {
		return "", fmt.Errorf("skillsource %s/%s credential %q: type=%s has no backing Secret to read a token from",
			src.Namespace, src.Name, cred.Name, cred.Type)
	}

	// Adopt the referenced Secret before reading it — metadata-only SSA that
	// stamps AdoptedLabel so subsequent reads are permitted. Adopt's existence
	// check uses the live reader (r.SecretReader.Reader) so a not-yet-adopted
	// Secret (absent from the label-filtered cache) is seen.
	ownerRef := types.NamespacedName{Namespace: src.Namespace, Name: src.Name}
	secretRef := types.NamespacedName{Namespace: src.Namespace, Name: ref.Name}
	if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "SkillSource"); err != nil {
		return "", fmt.Errorf("adopt Secret %s/%s: %w", src.Namespace, ref.Name, err)
	}

	// Value extraction is delegated to the kind's own ReadStoredValue (via
	// credresolve) rather than re-deriving each type's key convention here:
	// ref.Key is empty for a fixed multi-key shape (oauth), so indexing the
	// Secret with it directly would silently miss the real key. Reads use the
	// same live reader Adopt just used above — the adoption check moments ago
	// already established this Secret is ours to read.
	val, err := credresolve.ResolveSecretValue(ctx, r.SecretReader.Reader, src.Namespace, *cred)
	if err != nil {
		return "", fmt.Errorf("resolve credential %q: %w", cred.Name, err)
	}
	return string(val.UnderlyingValue()), nil
}

// upsertSkill creates or updates the owned Skill CR for a discovered skill and
// reports whether it wrote. The metadata.name is the canonical name's SafeSlug;
// the SkillSource is set as the controller owner so k8s GC prunes it on source
// deletion. Mirrors the get-or-create-or-update idiom in
// pkg/controllers/agentclass/grants.go.
func (r *Reconciler) upsertSkill(
	ctx context.Context, src *v1.SkillSource, d DiscoveredSkill,
	repoInstr *v1.SkillRepoInstructions, resolvedSHA string,
) (bool, error) {
	name, err := canonical.Parse(d.CanonicalName)
	if err != nil {
		return false, fmt.Errorf("parse canonical name %q: %w", d.CanonicalName, err)
	}
	slug := name.SafeSlug()

	spec := DesiredSkillSpec(
		SourceRef{RepoURL: src.Spec.RepoURL, Ref: src.Spec.Ref, Name: src.Name},
		d, repoInstr, resolvedSHA)

	desired := &v1.Skill{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slug,
			Namespace: src.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1.SchemeGroupVersion.String(),
				Kind:               "SkillSource",
				Name:               src.Name,
				UID:                src.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spec,
	}

	var existing v1.Skill
	err = r.Client.Get(ctx, client.ObjectKey{Name: slug, Namespace: src.Namespace}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create Skill/%s: %w", slug, err)
		}
	case err != nil:
		return false, fmt.Errorf("get Skill/%s: %w", slug, err)
	default:
		// Reconcile the owner reference: a pre-existing Skill with no/wrong owner
		// ref must be adopted so GC and self-heal work correctly.
		wantOwnerRef := desired.OwnerReferences[0]
		if skillspec.Equal(existing.Spec, spec) && skillspec.OwnerRefPresent(existing.OwnerReferences, wantOwnerRef) {
			return false, nil // no change
		}
		cp := existing.DeepCopy()
		cp.Spec = spec
		cp.OwnerReferences = skillspec.AdoptOwnerRef(cp.OwnerReferences, wantOwnerRef)
		if err := r.Client.Update(ctx, cp); err != nil {
			return false, fmt.Errorf("update Skill/%s: %w", slug, err)
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
func (r *Reconciler) requeue(ctx context.Context, src *v1.SkillSource, interval time.Duration) (ctrl.Result, error) {
	if err := r.Client.Status().Update(ctx, src); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: min(interval, failureRetryInterval)}, nil
}

// interval picks the effective re-poll cadence: spec.sync.interval, else the
// reconciler's SyncInterval, else the 1h default.
func (r *Reconciler) interval(src *v1.SkillSource) time.Duration {
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
		For(&v1.SkillSource{}).
		Owns(&v1.Skill{}).
		// The clone credential is two hops away (spec.auth → AgentIdentity →
		// Secret); without these a rotated PAT is invisible until the resync.
		// See watch.go.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToSources)).
		Watches(&v1.AgentIdentity{}, handler.EnqueueRequestsFromMapFunc(r.mapIdentityToSources)).
		Complete(r)
}
