// Package useridentity reconciles UserIdentity CRs — the cluster-scoped
// per-user credential catalog used by identityMode=userPassthrough agents.
// The validity reconciler validates spec shape and confirms every referenced
// Secret + key exists in the agentprimitives-identities namespace, surfacing
// Valid=True/False. Mirrors the AgentIdentity validity reconciler.
package useridentity

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=useridentities,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=useridentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=useridentities/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconciler reconciles UserIdentity objects.
type Reconciler struct {
	Client    client.Client
	APIReader client.Reader // non-cached, for non-secret reads (reserved for future use)
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader

	// RevokePublisher emits credential revokes on the unified ap.revocation
	// bus on credential removal or replacement. May be nil — controller
	// behaves identically, just without the side-effect emission. Production
	// wiring lives in internal/cmd/operator/main.go.
	RevokePublisher *RevokePublisher

	// SpiceDB writes the attested-identity edge binding a verified forge
	// account to this catalog's declared owner (attested_edge.go). Optional in
	// the same sense RevokePublisher is: nil skips the write with a log line,
	// which is the disconnected-cluster and test-fixture wiring. No status
	// condition depends on it — a SpiceDB outage must not hold a catalog
	// invalid.
	//
	// Declared as the INTERFACE, never *spicedb.Client, so a caller that never
	// assigns it leaves a genuine nil interface here rather than a typed-nil
	// pointer that passes `!= nil` and panics on first use (AGENTS.md's
	// nil-interface rule; pkg/controllers/agentclass's SpiceDBSchema is where
	// that bug actually shipped).
	SpiceDB AttestedIdentityWriter

	// MonitoringPublish reports a second claim on one provider account onto the
	// fixed monitoring subject, which channelsd fans out to role=monitoring
	// Channels. Nil when NATS is unconfigured; the conflict then goes
	// unreported, but the edge is still written — the notice is the best-effort
	// half. It is a func type, not an interface, so a nil here is a true nil.
	MonitoringPublish channelevents.PublishFunc

	// clock supplies the monitoring event's timestamp; nil defaults to
	// time.Now. Unexported because production has exactly one clock: this is a
	// test seam, not a dependency to wire.
	clock func() time.Time
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapSecretToUsers := func(ctx context.Context, o client.Object) []reconcile.Request {
		// UserIdentity is cluster-scoped; its credentials always live in
		// IdentitiesNamespace. Ignore Secret events from other namespaces.
		if o.GetNamespace() != spiceboxv1alpha1.IdentitiesNamespace {
			return nil
		}
		var list spiceboxv1alpha1.UserIdentityList
		if err := r.Client.List(ctx, &list); err != nil {
			log.FromContext(ctx).Info("list UserIdentities for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
				"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			u := &list.Items[i]
			for _, c := range u.Spec.Credentials {
				// Dispatch through the registry rather than switching on
				// c.Static/OAuth/Federated — see credkind/guard_test.go's Guard 6.
				// An unregistered type never matches this Secret; it is logged
				// (unlike "no backing Secret," that is a config problem this watch
				// would otherwise mask by silently never firing for it) and skipped
				// rather than aborting the whole mapper over one credential.
				name, err := credkindregistry.SecretNameFor(c)
				if err != nil {
					log.FromContext(ctx).Info("UserIdentity Secret watch: credential has an unregistered type; a rotation behind it will not be seen until the next resync",
						"userIdentity", u.Name, "credential", c.Name, "type", c.Type, "err", err.Error())
					continue
				}
				if name != "" && name == o.GetName() {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: u.Name}})
					break
				}
			}
		}
		return out
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.UserIdentity{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(mapSecretToUsers)).
		// A SECOND watch on the same GVK this controller is already `For`-ed
		// on: `For` enqueues the changed UserIdentity itself; this fans out to
		// every OTHER live UserIdentity that shares an attested account with
		// it (mapCoClaimants, attested_edge.go), so sole_user's derivation is
		// driven by a real event rather than the manager's own (unconfigured,
		// ~10h default) resync. controller-runtime shares one informer per
		// GVK across watches, so this is an extra event handler, not an extra
		// informer.
		Watches(&spiceboxv1alpha1.UserIdentity{}, handler.EnqueueRequestsFromMapFunc(r.mapCoClaimants)).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var u spiceboxv1alpha1.UserIdentity
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &u); !cont {
		return ctrl.Result{}, err
	}
	if u.DeletionTimestamp != nil {
		return ctrl.Result{}, r.finalize(ctx, &u)
	}
	// Hold the catalog open for as long as it can hold a durable identity edge.
	//
	// sole_user is derived from the credentials a pass READS, so the pass that
	// would withdraw it after the last claim goes away is the one deletion
	// prevents from ever running — see withdrawUnclaimedSoleIdentities. The
	// finalizer buys exactly that one pass.
	//
	// Only when SpiceDB is wired: with no writer there is no tuple to withdraw,
	// and a finalizer that can never have work to do is just a way to wedge a
	// deletion. Ensured BEFORE any edge is written, so a tuple can never exist
	// on a catalog that is not yet held open.
	//
	// Deliberately NOT returning a requeue when it is newly added, unlike the
	// other finalizer sites in this repo: the rest of this reconcile is the
	// pass that mints the edge, and skipping it here would leave the very first
	// reconcile of a linked credential writing nothing at all.
	if r.SpiceDB != nil {
		if _, err := apreconcile.EnsureFinalizer(ctx, r.Client, &u, spiceboxv1alpha1.FinalizerUserIdentity); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure UserIdentity finalizer: %w", err)
		}
	}
	// Snapshot pre-mutation status for MergeFrom patching, BEFORE the
	// revocation observation below stamps status.observedCredentials: a
	// snapshot taken afterwards would contain the stamp, compare equal, and
	// silently drop it from the patch.
	prior := u.DeepCopy()

	// Diff Spec.Credentials against the durable per-name fingerprint record
	// (status.observedCredentials, cached in-process); emit credential revokes
	// on the unified ap.revocation bus for REMOVED or REPLACED entries, and
	// stamp the advanced record for the status write below to persist. The
	// first observation of an identity primes without emitting.
	//
	// The error is CARRIED, not returned here: a revoke that failed to publish
	// must not stop this identity's status from converging. The publisher has
	// already held the failed entries at their previous value in both its cache
	// and the stamped record, so returning the error after the status write is
	// what turns "held back" into "retried" — the requeue is the only thing
	// that re-runs the diff.
	var revokeErr error
	if r.RevokePublisher != nil {
		revokeErr = r.RevokePublisher.Observe(ctx, &u)
	}

	// Joined, never preferred. reconcileStatus returns two different failures
	// through one return — a status write that did not land, and an
	// attested-identity edge that did not — and a pass where the revoke ALSO
	// failed would otherwise drop revokeErr unlogged, which is exactly the
	// silent-error shape this repo forbids. Both are retried by the same
	// requeue, so neither has to outrank the other; what matters is that both
	// reach the log.
	res, err := r.reconcileStatus(ctx, &u, prior)
	return res, errors.Join(err, revokeErr)
}

// finalize withdraws every sole_user edge this catalog's subject still holds
// and then releases the finalizer.
//
// A catalog on its way out claims NOTHING — hence the nil claim set — and
// liveAttestedClaimants skips CRs carrying a deletion timestamp, so this
// object does not answer "still claimed" on its own behalf. A remaining
// co-claimant of the same account still does, and keeps its tuple.
//
// A failed withdrawal KEEPS the finalizer: the object stays in Terminating
// until SpiceDB is reachable again. That is the fail-closed direction — the
// alternative is releasing the object while the durable grant it minted is
// still live, with nothing left that knows the tuple exists.
//
// The one case that releases without withdrawing is r.SpiceDB == nil, which
// withdrawUnclaimedSoleIdentities treats as a no-op. An install that wrote
// edges and later lost its SpiceDB wiring would leak the tuple there, so it is
// logged rather than silent.
func (r *Reconciler) finalize(ctx context.Context, u *spiceboxv1alpha1.UserIdentity) error {
	if !controllerutil.ContainsFinalizer(u, spiceboxv1alpha1.FinalizerUserIdentity) {
		return nil
	}
	if r.SpiceDB == nil {
		log.FromContext(ctx).Info("useridentity: deleted with no SpiceDB wiring; any sole_user edge this catalog minted is left in place",
			"userIdentity", u.Name, "subject", u.Spec.Subject)
	}
	if err := r.withdrawUnclaimedSoleIdentities(ctx, u, nil); err != nil {
		return fmt.Errorf("withdraw sole identity edges before deleting UserIdentity %s: %w", u.Name, err)
	}
	controllerutil.RemoveFinalizer(u, spiceboxv1alpha1.FinalizerUserIdentity)
	return r.Client.Update(ctx, u)
}

// reconcileStatus is spec validation, Secret resolution, and the single status
// patch. Split out so Reconcile can run it before deciding what to return —
// see the revokeErr comment there.
//
// prior is the caller's pre-mutation snapshot, threaded in rather than taken
// here so the revocation observation's stamp is part of the diff.
//
// Every setInvalid return below precedes the attested-edge pass, so ONE
// unresolvable Secret suppresses the edge for that catalog's healthy
// credentials too. Level-triggered and self-correcting — fixing the broken
// credential re-runs the pass — and reordering would mean writing identity
// edges from a catalog the reconciler has just declared invalid.
func (r *Reconciler) reconcileStatus(ctx context.Context, u *spiceboxv1alpha1.UserIdentity,
	prior *spiceboxv1alpha1.UserIdentity) (ctrl.Result, error) {
	// Spec-shape: each credential must declare a name + a valid type-specific block.
	if reason, msg := validateSpecShape(&u.Spec); reason != "" {
		return r.setInvalid(ctx, u, prior, reason, msg)
	}

	// Existence: every credential's Secret + key must resolve in IdentitiesNamespace.
	// Each Secret is adopted before being read so the adoptguard SecretReader
	// permits the access.
	ownerRef := types.NamespacedName{Name: u.Name} // cluster-scoped; no namespace
	resolved := make(map[string]bool, len(u.Spec.Credentials))
	// Every credential Secret this pass read and validated, in credential
	// order, handed to the attested-identity edge writer below. Carried out of
	// the loop rather than written from inside it so a SpiceDB failure cannot
	// pre-empt the status convergence the rest of this function owes the CR —
	// see the edge-error handling after the loop.
	var attestedSecrets []*corev1.Secret
	for _, c := range u.Spec.Credentials {
		k, err := credkindregistry.Get(c.Type)
		if err != nil {
			// validateSpecShape above already rejected every credential whose type
			// the registry does not know, so this is unreachable in practice — but
			// it must still fail closed rather than panic on a nil Kind.
			return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("credentials[%s]: %v", c.Name, err))
		}
		ref := k.SecretRef(c)
		if ref == nil {
			resolved[c.Name] = true // nothing stored, nothing to check — trivially resolved
			continue
		}
		secretName := ref.Name
		secretRef := types.NamespacedName{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secretName}
		// Adopt the referenced Secret — metadata-only SSA that stamps AdoptedLabel
		// so subsequent reads via SecretReader are permitted. Adopt's existence
		// check uses the live reader (r.SecretReader.Reader), so a not-yet-adopted
		// Secret (absent from the label-filtered cache) is seen. A NotFound from
		// adopt drives the user-visible SecretMissing reason — the same signal the
		// dropped cached pre-check produced — without silently minting an empty
		// Secret in the identities namespace via the SSA apply.
		if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "UserIdentity"); err != nil {
			if apierrors.IsNotFound(err) {
				return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonSecretMissing,
					fmt.Sprintf("credentials[%s]: Secret %q not found in %s", c.Name, secretName, spiceboxv1alpha1.IdentitiesNamespace))
			}
			log.FromContext(ctx).Info("adoptkit.AdoptSecret failed, requeuing", "secret", secretRef, "err", err)
			return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonSecretMissing,
				fmt.Sprintf("credentials[%s]: adopt Secret %q: %v", c.Name, secretName, err))
		}
		sec, err := r.SecretReader.Get(ctx, secretRef)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonSecretMissing,
					fmt.Sprintf("credentials[%s]: Secret %q not found in %s", c.Name, secretName, spiceboxv1alpha1.IdentitiesNamespace))
			}
			return ctrl.Result{}, err
		}
		// Every key this type's Secret shape requires — static's single named
		// key, oauth's access_token, or a minted multi-key kind's own fields (a
		// GitHub App's app-id/private-key/installation-id) — is checked
		// generically here, from the kind itself, instead of a hand-rolled
		// per-type case. NeedsRefresh() keeps oauth's own historical reason and
		// message shape (only oauth is NeedsRefresh() today), so this credential
		// still reports OAuthSecretIncomplete rather than the generic
		// SecretKeyMissing/CredentialEmpty pair every other type reports.
		for _, key := range k.RequiredSecretKeys(c) {
			val, ok := sec.Data[key]
			if ok && len(val) > 0 {
				continue
			}
			if k.NeedsRefresh() {
				return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonOAuthSecretIncomplete,
					fmt.Sprintf("credentials[%s]: Secret %q has no/empty %s key", c.Name, secretName, key))
			}
			if !ok {
				return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonSecretKeyMissing,
					fmt.Sprintf("credentials[%s]: Secret %q has no key %q", c.Name, secretName, key))
			}
			return r.setInvalid(ctx, u, prior, spiceboxv1alpha1.ReasonCredentialEmpty,
				fmt.Sprintf("credentials[%s]: Secret %q has empty key %q", c.Name, secretName, key))
		}
		resolved[c.Name] = true
		attestedSecrets = append(attestedSecrets, sec)
	}

	// Mint the attested-identity edge for every credential whose Secret carries
	// a link-time attestation, from the Secrets already in hand. Run BEFORE the
	// no-change short-circuit below, because a converged status is exactly the
	// steady state in which a re-observed attestation still has to converge in
	// SpiceDB.
	//
	// The error is CARRIED, not returned here, for the same reason revokeErr is
	// in Reconcile: a lost edge must requeue, but it must not stop this
	// identity's status from converging — a SpiceDB outage does not make the
	// catalog invalid.
	edgeErr := r.reconcileAttestedEdges(ctx, u, attestedSecrets)

	u.Status.ObservedGeneration = u.Generation
	u.Status.ResolvedCredentials = u.Status.ResolvedCredentials[:0]
	u.Status.AvailableCredentials = u.Status.AvailableCredentials[:0]
	for _, c := range u.Spec.Credentials {
		if resolved[c.Name] {
			u.Status.ResolvedCredentials = append(u.Status.ResolvedCredentials, c.Name)
			u.Status.AvailableCredentials = append(u.Status.AvailableCredentials, c.Name)
		}
	}

	conditions.SetTrue(u, &u.Status.Conditions,
		spiceboxv1alpha1.UserIdentityConditionValid, spiceboxv1alpha1.ReasonAllReferencesResolve)
	if equality.Semantic.DeepEqual(prior.Status, u.Status) {
		return ctrl.Result{}, edgeErr
	}
	if err := r.Client.Status().Patch(ctx, u, client.MergeFrom(prior)); err != nil {
		// Joined for the same reason Reconcile joins revokeErr: the requeue
		// this earns re-runs the edge write too, so neither failure needs to
		// outrank the other — but returning the patch error alone would drop
		// the edge failure with nothing logged.
		return ctrl.Result{}, errors.Join(err, edgeErr)
	}
	return ctrl.Result{}, edgeErr
}

// setInvalid stamps Valid=False and patches status against the caller's
// pre-mutation snapshot. prior is threaded in rather than taken here so that
// every status change made earlier in the reconcile — notably the revocation
// observation's status.observedCredentials stamp — is part of the patch. A
// locally-taken snapshot would already contain those changes and drop them,
// which for the revocation record means the identity's trigger state silently
// stops advancing for exactly the identities whose credentials are broken.
func (r *Reconciler) setInvalid(ctx context.Context, u, prior *spiceboxv1alpha1.UserIdentity,
	reason, msg string) (ctrl.Result, error) {
	u.Status.ResolvedCredentials = nil
	u.Status.AvailableCredentials = nil
	return apreconcile.SetInvalid(ctx, u, &u.Status.ObservedGeneration, &u.Status.Conditions,
		spiceboxv1alpha1.UserIdentityConditionValid, reason, msg,
		func(ctx context.Context) error { return r.Client.Status().Patch(ctx, u, client.MergeFrom(prior)) })
}

// validateSpecShape returns a non-empty (reason, message) when the spec is
// shape-invalid. All checks are deterministic and do not hit the API server.
func validateSpecShape(s *spiceboxv1alpha1.UserIdentitySpec) (string, string) {
	seenCred := map[string]bool{}
	for i, c := range s.Credentials {
		if c.Name == "" {
			return spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("credentials[%d].name is empty", i)
		}
		if seenCred[c.Name] {
			return spiceboxv1alpha1.ReasonSpecInvalid,
				fmt.Sprintf("credentials[%s]: duplicate credential name", c.Name)
		}
		seenCred[c.Name] = true

		// Registered, legal on this scope, own block well-formed, and no
		// sibling type's block set — one call, so a rule added there reaches
		// this reconciler and AgentIdentity's alike. See
		// credkindregistry.ValidateCredential.
		if err := credkindregistry.ValidateCredential(c, credkind.ScopeUserIdentity); err != nil {
			return spiceboxv1alpha1.ReasonSpecInvalid, err.Error()
		}
	}
	return "", ""
}
