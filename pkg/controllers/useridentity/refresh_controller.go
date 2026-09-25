// Package useridentity refresh_controller.go: the RefreshReconciler
// proactively refreshes type=oauth credentials within a configurable
// threshold of expiry. Sibling to the validity Reconciler in this
// package; the two reconcilers split responsibility for Valid (read-
// only) and Refresh (Secret-write) status conditions on UserIdentity.
//
// The refresh policy itself lives in
// pkg/controllers/internal/identityrefresh, shared with the AgentIdentity
// refresh reconciler; what stays here is the manager wiring, the
// Secret→UserIdentity watch mapping, and the adapter that tells the shared
// policy where a UserIdentity's credential Secrets live.
package useridentity

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/identityrefresh"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	// Aliased: this controller package is also named useridentity.
	identityuser "github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// The refreshed access_token / refresh_token / expires_at are written back
// to the credential Secret by pkg/platform/identity/refresh.Run, which is handed the
// operator's client and uses a MergeFrom client.Patch — `patch` only, never
// client.Update. (Secret `get`/`list`/`watch` for this controller are
// covered by the useridentity validity reconciler's read grant.)
//
// `create` + `update` are for the redemption-material migration, not the
// refresh: it creates a pre-split credential's sibling Secret and updates the
// master to strip the co-located copy. See migrateRedemptionMaterial.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;patch;update

// RefreshReconciler runs RFC 6749 refresh-token grants for type=oauth
// UserIdentity credentials approaching expiry. Sibling to the
// validity Reconciler in this package.
type RefreshReconciler struct {
	Client    client.Client
	APIReader client.Reader // non-cached, for non-secret reads (reserved for future use)
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. The guard's internal APIReader preserves the live-read
	// intent of the previous r.APIReader.Get path for oauth Secret reads.
	SecretReader     *adoptguard.SecretReader
	DefaultThreshold time.Duration

	coreOnce sync.Once
	shared   *identityrefresh.Core
}

// core lazily builds the shared refresh policy from the exported fields the
// operator sets. Built under a sync.Once rather than only in
// SetupWithManager so a reconciler constructed as a struct literal — every
// test does this — can never reach the refresh path with a nil backoff and
// panic.
func (r *RefreshReconciler) core() *identityrefresh.Core {
	r.coreOnce.Do(func() {
		r.shared = identityrefresh.New(r.Client, r.SecretReader, r.DefaultThreshold)
	})
	return r.shared
}

// SetupWithManager registers the reconciler with mgr. Same watches as
// the validity reconciler, but a distinct controller name so leader-
// election + metrics are scoped independently.
func (r *RefreshReconciler) SetupWithManager(mgr ctrl.Manager) error {
	_ = r.core() // build eagerly; the first reconcile then does no setup work
	return ctrl.NewControllerManagedBy(mgr).
		Named("useridentity-refresh").
		For(&spiceboxv1alpha1.UserIdentity{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToUsers),
		).
		Complete(r)
}

// mapSecretToUsers enqueues every UserIdentity that references the Secret as
// an oauth credential. UserIdentity is cluster-scoped and all its credential
// Secrets live in IdentitiesNamespace, so the list is cluster-wide behind a
// namespace pre-filter: without it, a same-named Secret in any other
// namespace would enqueue every UserIdentity referencing that name.
func (r *RefreshReconciler) mapSecretToUsers(ctx context.Context, o client.Object) []reconcile.Request {
	if o.GetNamespace() != spiceboxv1alpha1.IdentitiesNamespace {
		return nil
	}
	var list spiceboxv1alpha1.UserIdentityList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("refresh: list UserIdentities for Secret watch failed",
			"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		u := &list.Items[i]
		if identityrefresh.ReferencesSecret(u.Spec.Credentials, o.GetName()) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(u)})
		}
	}
	return out
}

// Reconcile is the entrypoint. See the spec for the full state machine.
//
// The redemption-material migration runs FIRST, before the shared refresh core:
// it is the operator's only chance to split a pre-split credential before the
// runner next reaches for it, and the core's own refresh reads whichever shape
// it leaves behind.
func (r *RefreshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.migrateRedemptionMaterial(ctx, req.NamespacedName)
	var u spiceboxv1alpha1.UserIdentity
	return r.core().Reconcile(ctx, req.NamespacedName, refreshTarget{&u})
}

// migrateRedemptionMaterial moves every oauth credential's co-located RFC 6749
// redemption material into its sibling Secret — the upgrade path for
// credentials linked before the Secret split, which nothing else migrates
// (pkg/platform/identity/useridentity.PutOAuthToken strips the co-located copy only on a
// manual re-link, and nobody re-links a working credential).
//
// This reconciler owns it because it is the component that both may read the
// material and may write both Secrets. A userPassthrough runner has neither
// grant, and pkg/platform/identity/refresh now refuses to redeem from co-located keys
// when the sibling is unreadable — so until this has run, an expiring pre-split
// credential surfaces a loud refusal runner-side and is healed here, rather than
// being redeemed by a caller that cannot persist the rotated token.
//
// A failure is logged and the reconcile CONTINUES: the credential is left in its
// working pre-split shape, which the operator's own refresh path still handles
// via the co-located fallback, so blocking the refresh on a migration failure
// would turn a hardening gap into an outage. What is lost until the next pass is
// the hardening, and that is what the log line says.
func (r *RefreshReconciler) migrateRedemptionMaterial(ctx context.Context, key types.NamespacedName) {
	logger := log.FromContext(ctx)
	var u spiceboxv1alpha1.UserIdentity
	if err := r.Client.Get(ctx, key, &u); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Info("refresh: reading UserIdentity for redemption-material migration failed",
				"userIdentity", key.Name, "err", err.Error())
		}
		return
	}
	if u.DeletionTimestamp != nil {
		return
	}
	for _, cred := range u.Spec.Credentials {
		k, err := credkindregistry.Get(cred.Type)
		if err != nil {
			// The validity Reconciler's validateSpecShape already rejects any
			// credential whose type the registry does not know, so this is
			// unreachable once that reconciler has caught up — but the two
			// reconcilers run independently, so a freshly-created UserIdentity
			// can reach here first. Fail closed (skip) rather than panic.
			logger.Info("refresh: skipping credential of unknown type during redemption-material migration",
				"userIdentity", key.Name, "credential", cred.Name, "type", cred.Type, "err", err.Error())
			continue
		}
		if !k.NeedsRefresh() || cred.OAuth == nil || cred.OAuth.SecretRef.Name == "" {
			continue
		}
		moved, err := identityuser.MigrateRedemptionMaterial(ctx, r.Client,
			spiceboxv1alpha1.IdentitiesNamespace, cred.OAuth.SecretRef.Name)
		if err != nil {
			logger.Info("refresh: moving co-located OAuth redemption material into its refresh-material "+
				"Secret failed; the credential still refreshes, but a passthrough runner granted `get` on "+
				"the master can still read the material to redeem its refresh token",
				"userIdentity", key.Name, "credential", cred.Name,
				"secret", cred.OAuth.SecretRef.Name, "err", err.Error())
			continue
		}
		if moved {
			logger.Info("refresh: moved co-located OAuth redemption material into its refresh-material Secret",
				"userIdentity", key.Name, "credential", cred.Name, "secret", cred.OAuth.SecretRef.Name)
		}
	}
}

// refreshTarget adapts *UserIdentity to identityrefresh.Identity.
type refreshTarget struct {
	*spiceboxv1alpha1.UserIdentity
}

var _ identityrefresh.Identity = refreshTarget{}

func (t refreshTarget) Object() client.Object { return t.UserIdentity }
func (t refreshTarget) Kind() string          { return "UserIdentity" }

// SecretNamespace: a UserIdentity is cluster-scoped and all its oauth
// Secrets live in the fixed identities namespace.
func (t refreshTarget) SecretNamespace() string {
	return spiceboxv1alpha1.IdentitiesNamespace
}

func (t refreshTarget) Credentials() []spiceboxv1alpha1.AgentCredential {
	return t.Spec.Credentials
}
func (t refreshTarget) ThresholdOverride() *metav1.Duration { return t.Spec.RefreshThreshold }
func (t refreshTarget) ConditionType() string {
	return spiceboxv1alpha1.UserIdentityConditionRefresh
}
func (t refreshTarget) StatusConditions() *[]metav1.Condition { return &t.Status.Conditions }
func (t refreshTarget) SetLastRefreshAt(at metav1.Time)       { t.Status.LastRefreshAt = &at }
func (t refreshTarget) StatusSnapshot() any                   { return t.Status }
func (t refreshTarget) DeepCopyIdentity() identityrefresh.Identity {
	return refreshTarget{t.UserIdentity.DeepCopy()}
}
