// Package agentidentity refresh_controller.go: the RefreshReconciler
// proactively refreshes type=oauth credentials within a configurable
// threshold of expiry. Sibling to the validity Reconciler in this
// package; the two reconcilers split responsibility for Valid (read-
// only) and Refresh (Secret-write) status conditions on AgentIdentity.
//
// The refresh policy itself lives in
// pkg/controllers/internal/identityrefresh, shared with the UserIdentity
// refresh reconciler; what stays here is the manager wiring, the
// Secret→AgentIdentity watch mapping, and the adapter that tells the shared
// policy where an AgentIdentity's credential Secrets live.
package agentidentity

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/identityrefresh"
)

// The refreshed access_token / refresh_token / expires_at are written back
// to the credential Secret by pkg/platform/identity/refresh.Run, which is handed the
// operator's client and uses a MergeFrom client.Patch — `patch` only, never
// client.Update. (Secret `get`/`list`/`watch` for this controller are
// covered by the agentidentity validity reconciler's read grant.)
// +kubebuilder:rbac:groups="",resources=secrets,verbs=patch

// RefreshReconciler runs RFC 6749 refresh-token grants for type=oauth
// AgentIdentity credentials approaching expiry. Sibling to the
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
		Named("agentidentity-refresh").
		For(&spiceboxv1alpha1.AgentIdentity{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToAgents),
		).
		Complete(r)
}

// mapSecretToAgents enqueues every AgentIdentity in the Secret's own
// namespace that references it as an oauth credential. AgentIdentity is
// namespaced and its credential Secrets are same-namespace, so the list is
// scoped to that namespace.
func (r *RefreshReconciler) mapSecretToAgents(ctx context.Context, o client.Object) []reconcile.Request {
	var list spiceboxv1alpha1.AgentIdentityList
	if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
		log.FromContext(ctx).Info("refresh: list AgentIdentities for Secret watch failed",
			"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		a := &list.Items[i]
		if identityrefresh.ReferencesSecret(a.Spec.Credentials, o.GetName()) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
		}
	}
	return out
}

// Reconcile is the entrypoint. See the spec for the full state machine.
func (r *RefreshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a spiceboxv1alpha1.AgentIdentity
	return r.core().Reconcile(ctx, req.NamespacedName, refreshTarget{&a})
}

// refreshTarget adapts *AgentIdentity to identityrefresh.Identity.
type refreshTarget struct {
	*spiceboxv1alpha1.AgentIdentity
}

var _ identityrefresh.Identity = refreshTarget{}

func (t refreshTarget) Object() client.Object { return t.AgentIdentity }
func (t refreshTarget) Kind() string          { return "AgentIdentity" }

// SecretNamespace: an AgentIdentity's oauth Secrets are in its own namespace.
func (t refreshTarget) SecretNamespace() string { return t.Namespace }

func (t refreshTarget) Credentials() []spiceboxv1alpha1.AgentCredential {
	return t.Spec.Credentials
}
func (t refreshTarget) ThresholdOverride() *metav1.Duration { return t.Spec.RefreshThreshold }
func (t refreshTarget) ConditionType() string {
	return spiceboxv1alpha1.AgentIdentityConditionRefresh
}
func (t refreshTarget) StatusConditions() *[]metav1.Condition { return &t.Status.Conditions }
func (t refreshTarget) SetLastRefreshAt(at metav1.Time)       { t.Status.LastRefreshAt = &at }
func (t refreshTarget) StatusSnapshot() any                   { return t.Status }
func (t refreshTarget) DeepCopyIdentity() identityrefresh.Identity {
	return refreshTarget{t.AgentIdentity.DeepCopy()}
}
