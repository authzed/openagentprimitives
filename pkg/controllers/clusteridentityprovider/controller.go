// pkg/controllers/clusteridentityprovider/controller.go
//
// Package clusteridentityprovider reconciles the ClusterIdentityProvider
// singleton ("default"). It sets the Valid condition based on sequential
// gates:
//
//  1. generic spec validity — delegates to webhooksettings.IdPSpecError so
//     the webhook and controller share the same judgment and can never
//     disagree.
//  2. kind resolvable — spec.Kind must name a registered idp.Kind
//     (pkg/platform/identity/idp/registry).
//  3. local-only kind admissibility — delegates to Kind.AllowedNonLocal; a
//     kind that returns false (e.g. password, a single-user local admin
//     gate with no anti-brute-force posture of its own) is refused unless
//     Reconciler.LocalCluster is true. Checked before any other kind-
//     specific judgment so an insecure kind fails fast.
//  4. kind-specific spec validity — delegates to Kind.ValidateSpec, e.g.
//     password requires allowAnyEmail=true (a password kind has no email
//     domains).
//  5. client-secret resolvability — the referenced Secret and key must exist
//     and be non-empty.
//  6. discovery reachability — delegates to Kind.DiscoveryURL for the probe
//     target; GET <url> must return 2xx. A kind whose DiscoveryURL is ""
//     (e.g. password, a local non-federated IdP with no remote issuer) skips
//     this gate entirely — gates 1-5 are sufficient for it.
//
// The controller holds NO per-kind branches: every kind-specific judgment
// (local-only admissibility, spec validity beyond the generic webhook rule,
// and the discovery probe target) is dispatched through the idp.Kind
// interface via the registry, so adding a new kind never touches this file.
//
// identityd (a later task) requires Valid=True before serving login.
package clusteridentityprovider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	webhooksettings "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// discoveryClient is the SSRF-guarded client for OIDC discovery probes. The
// issuer URL is user-supplied (ClusterIdentityProvider.spec.issuer), so the
// reconciler must refuse private/loopback/link-local targets — same guard as
// the login-time provider in pkg/platform/identity/idp/oidckind. Tests inject their own
// client via HTTPClient to reach a loopback httptest server.
func discoveryClient() *http.Client {
	c := safehttp.Client()
	c.Timeout = 10 * time.Second
	return c
}

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteridentityproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteridentityproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconciler reconciles ClusterIdentityProvider objects.
type Reconciler struct {
	Client client.Client
	// HTTPClient is used for OIDC discovery probes. When nil,
	// SetupWithManager installs a http.Client with a 10s timeout.
	// Inject a test-scoped client to avoid real network calls in tests.
	HTTPClient *http.Client
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader
	// LocalCluster reports whether this operator is running against a local
	// (oap init --local / oap desktop) cluster rather than a real/public one. It
	// gates idp.Kind.AllowedNonLocal()==false kinds (e.g. password): those are
	// only ever valid when LocalCluster is true. main.go sets this from
	// cloud.Strategy.InstallProfile().AllowsLocalOnlyIdentityProviders(), where
	// the Strategy is resolved fail-closed from the AP_CLUSTER_KIND `oap
	// install` stamped onto the Deployment — see
	// internal/cmd/operator/main.go's resolveClusterKindFromEnv. It is NOT derived from
	// the memory backend: an install can run Postgres locally or sqlite
	// remotely, and the two facts must not be conflated.
	//
	// TODO(cluster-kind-registry): rename this field to something describing
	// its meaning (e.g. AllowLocalOnlyKinds) rather than its old derivation.
	LocalCluster bool
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("clusteridentityprovider", req.Name)

	var cidp v1.ClusterIdentityProvider
	if err := r.Client.Get(ctx, req.NamespacedName, &cidp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	prior := cidp.DeepCopy()

	result, condStatus, reason, message := r.evaluate(ctx, &cidp)

	cidp.Status.ObservedGeneration = cidp.Generation
	if condStatus == metav1.ConditionTrue {
		conditions.SetTrue(&cidp, &cidp.Status.Conditions, v1.ConditionIdPValid, reason)
	} else {
		conditions.SetFalse(&cidp, &cidp.Status.Conditions, v1.ConditionIdPValid, reason, message)
	}

	if equality.Semantic.DeepEqual(prior.Status, cidp.Status) {
		logger.V(1).Info("status unchanged, skipping write", "valid", condStatus == metav1.ConditionTrue, "reason", reason)
		return result, nil
	}
	if err := r.Client.Status().Patch(ctx, &cidp, client.MergeFrom(prior)); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled", "valid", condStatus == metav1.ConditionTrue, "reason", reason)
	return result, nil
}

// evaluate runs the gate sequence documented in the package comment and
// returns the reconcile result plus the condition fields to stamp.
func (r *Reconciler) evaluate(ctx context.Context, cidp *v1.ClusterIdentityProvider) (ctrl.Result, metav1.ConditionStatus, string, string) {
	// Gate 1: generic spec validity, shared with the webhook.
	if msg := webhooksettings.IdPSpecError(&cidp.Spec); msg != "" {
		return ctrl.Result{}, metav1.ConditionFalse,
			v1.ReasonIdPConfigInvalid, msg
	}

	// Gate 2: kind resolvable. Every kind-specific judgment from here on is
	// dispatched through this Kind — no string-compare on cidp.Spec.Kind
	// below.
	kind, ok := registry.Get(cidp.Spec.Kind)
	if !ok {
		return ctrl.Result{}, metav1.ConditionFalse,
			v1.ReasonIdPConfigInvalid,
			fmt.Sprintf("unknown spec.kind %q (registered: %s)", cidp.Spec.Kind, strings.Join(registry.Names(), ", "))
	}

	// Gate 3: local-only kind admissibility. A kind whose AllowedNonLocal()
	// is false (e.g. password — no external issuer, no anti-brute-force
	// posture, unauthenticated /password/verify) is only ever valid on a
	// local cluster. Checked before any other kind-specific judgment
	// (spec/secret/discovery) so an insecure kind fails fast, and via the
	// interface — never a Spec.Kind string compare — so a future local-only
	// kind is fenced off automatically.
	if !kind.AllowedNonLocal() && !r.LocalCluster {
		return ctrl.Result{}, metav1.ConditionFalse,
			v1.ReasonIdPConfigInvalid,
			fmt.Sprintf("kind %q is only permitted on a local (oap init --local) cluster", cidp.Spec.Kind)
	}

	// Gate 4: kind-specific spec validity (e.g. password requires
	// allowAnyEmail=true — a password kind has no email domains).
	if msg := kind.ValidateSpec(cidp.Spec); msg != "" {
		return ctrl.Result{}, metav1.ConditionFalse,
			v1.ReasonIdPConfigInvalid, msg
	}

	// Gate 5: client-secret resolvable.
	ref := cidp.Spec.ClientSecretRef
	secretRef := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	ownerRef := types.NamespacedName{Name: cidp.Name} // cluster-scoped; no namespace

	// Adopt the referenced secret before reading it — metadata-only SSA that
	// stamps AdoptedLabel so subsequent reads via SecretReader are permitted.
	// The existence check inside Adopt uses the live reader (r.SecretReader.Reader)
	// so a not-yet-adopted Secret (absent from the label-filtered cache) is seen.
	if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "ClusterIdentityProvider"); err != nil {
		logger := log.FromContext(ctx).WithValues("clusteridentityprovider", cidp.Name)
		logger.Info("adoptkit.AdoptSecret failed, requeuing", "secret", secretRef, "err", err)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, metav1.ConditionFalse,
			v1.ReasonIdPSecretMissing,
			fmt.Sprintf("adopt Secret %s/%s: %v", ref.Namespace, ref.Name, err)
	}

	sec, err := r.SecretReader.Get(ctx, secretRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, metav1.ConditionFalse,
				v1.ReasonIdPSecretMissing,
				fmt.Sprintf("Secret %s/%s not found", ref.Namespace, ref.Name)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, metav1.ConditionFalse,
			v1.ReasonIdPSecretMissing,
			fmt.Sprintf("get Secret %s/%s: %v", ref.Namespace, ref.Name, err)
	}
	if val, ok := sec.Data[ref.Key]; !ok || len(val) == 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, metav1.ConditionFalse,
			v1.ReasonIdPSecretMissing,
			fmt.Sprintf("Secret %s/%s missing or empty key %q", ref.Namespace, ref.Name, ref.Key)
	}

	// Gate 6: discovery reachable, per the kind's own DiscoveryURL. A kind
	// that needs no remote issuer (e.g. password — identityd serves its
	// login form itself, see pkg/platform/identity/idp/passwordkind) returns "" and is
	// ready as soon as gate 5 resolves its Secret.
	discoveryURL := kind.DiscoveryURL(cidp.Spec)
	if discoveryURL == "" {
		return ctrl.Result{}, metav1.ConditionTrue, v1.ReasonIdPReady, ""
	}

	hc := r.HTTPClient
	if hc == nil {
		hc = discoveryClient()
	}

	resp, err := hc.Get(discoveryURL) //nolint:noctx // uses caller ctx only for k8s ops; HTTP probe is best-effort
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, metav1.ConditionFalse,
			v1.ReasonIdPDiscoveryFailed,
			fmt.Sprintf("GET %s: %v", discoveryURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ctrl.Result{RequeueAfter: time.Minute}, metav1.ConditionFalse,
			v1.ReasonIdPDiscoveryFailed,
			fmt.Sprintf("GET %s: unexpected status %d", discoveryURL, resp.StatusCode)
	}

	return ctrl.Result{}, metav1.ConditionTrue, v1.ReasonIdPReady, ""
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	if r.HTTPClient == nil {
		r.HTTPClient = discoveryClient()
	}
	// SecretReader is injected from main.go (operator-wide, flag-controlled guard
	// mode). Tests inject their own reader directly. No self-construct here.
	// Secret-watching is deferred: a ClusterIdentityProvider references a
	// Secret in an arbitrary namespace. Watching all Secrets cluster-wide
	// adds significant informer overhead. The controller instead re-converges
	// via a 30s RequeueAfter on every SecretMissing path (absent Secret,
	// absent key, or transient get-error), so a Secret created after the CR
	// will be picked up within 30 seconds without manual intervention.
	// DiscoveryFailed cases requeue after 1 minute. A targeted Secret watch
	// can be added later if operator UX warrants it.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ClusterIdentityProvider{}).
		Complete(r)
}
