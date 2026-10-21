// Package agentidentity reconciles AgentIdentity CRs. The reconciler validates
// spec shape, confirms every referenced Secret + key exists, checks each stored
// value against the token format its provider declares, and surfaces
// Valid=True/False with a reason. A Valid=False AgentIdentity causes a
// referencing ToolCall to requeue rather than permanently fail (cache-lag
// tolerance).
//
// Value bytes are inspected, never retained or surfaced: they are read wrapped
// in a sensitive.SensitiveValue, tested for emptiness and against the declared
// format, and dropped. No condition message, log line or status field carries a
// credential value.
package agentidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentidentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// checkCredentialShape resolves a credential name to its provider through the
// cluster's MCPServers when no toolkit declares it.
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=mcpservers,verbs=get;list;watch

// secretArrivalRequeue is how long a Valid=False/SecretMissing identity waits
// before its credential Secrets are looked for again.
//
// The Secret watch below cannot cover this state, and the gap is
// SELF-SUSTAINING rather than merely slow. The operator's manager cache
// label-filters the Secret informer on adoptguard.AdoptedLabel, and that label
// is stamped only BY a reconcile (adoptkit.AdoptSecret). So a Secret created
// after the AgentIdentity — `kubectl create secret generic ...`, the documented
// install step, or a delete-and-recreate to rotate a token — carries no label,
// fires no event, gets no reconcile, and is never labelled. Nothing but the
// manager's full resync (10h by default) breaks the cycle. This timer is what
// closes it.
//
// The value trades reconcile churn against how long a correct install looks
// broken. The reconcile it drives is one live Get per credential, and it only
// runs while the identity is already Valid=False, so the cost of a
// permanently-missing Secret is bounded and small; a human who has just run
// `kubectl create secret` is the case being optimized for.
const secretArrivalRequeue = 30 * time.Second

// PlatformLinker is the reconciler's narrow view of SpiceDB: the single write
// that links an AgentIdentity to the singleton platform object. Satisfied by
// *spicedb.Client.
//
// An implementation MUST report a name that can never be linked — one outside
// SpiceDB's object_id charset — as spicedb.ErrUnrepresentableObjectID. The
// reconciler branches on it to stop retrying and surface the cause on the CR;
// returned as any other error it would requeue forever against a condition no
// retry can change.
type PlatformLinker interface {
	EnsureAgentIdentityPlatform(ctx context.Context, ns, name string) error
}

// Reconciler reconciles AgentIdentity objects.
type Reconciler struct {
	Client    client.Client
	APIReader client.Reader // non-cached, for non-secret reads (reserved for future use)
	// PlatformLinker writes agentidentity:<ns>/<name>#platform@platform:platform,
	// the ONLY thing that makes agentidentity#update_credential satisfiable (the
	// permission's other arm, `editor`, ships deliberately unpopulated — see the
	// agentidentity definition in pkg/authz/spicedb/schema/schema.zed). Without it a
	// platform admin cannot replace a dead bot credential and the refusal is
	// silent. internal/cmd/operator wires the *spicedb.Client; nil in test fixtures
	// without SpiceDB, which is logged rather than silently skipped.
	//
	// Declared as the INTERFACE, never as *spicedb.Client: a typed-nil pointer
	// assigned into an interface field yields a NON-nil interface that panics on
	// first call (AGENTS.md, "Nil interfaces"). Callers must assign only from a
	// real value.
	PlatformLinker PlatformLinker
	// SecretReader is the guarded Secret reader. Reads are gated to secrets
	// the operator has adopted (carrying AdoptedLabel) or the fixed-infra
	// allowlist. Must be set before Reconcile is called.
	SecretReader *adoptguard.SecretReader
	// RevokePublisher diffs credentials between reconciles and emits
	// credential revocations on the unified ap.revocation bus. Optional:
	// when nil, revocations are skipped (no-NATS local-dev flows).
	RevokePublisher *RevokePublisher
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapSecretToAgents := func(ctx context.Context, o client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.AgentIdentityList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
			log.FromContext(ctx).Info("list AgentIdentities for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
				"secret", o.GetName(), "namespace", o.GetNamespace(), "err", err.Error())
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			a := &list.Items[i]
			for _, c := range a.Spec.Credentials {
				// Dispatch through the registry rather than switching on
				// c.Static/OAuth/Federated — see credkind/guard_test.go's Guard 6.
				// An unregistered type never matches this Secret; it is logged
				// (unlike "no backing Secret," that is a config problem this watch
				// would otherwise mask by silently never firing for it) and skipped
				// rather than aborting the whole mapper over one credential.
				name, err := credkindregistry.SecretNameFor(c)
				if err != nil {
					log.FromContext(ctx).Info("AgentIdentity Secret watch: credential has an unregistered type; a rotation behind it will not be seen until the next resync",
						"agentIdentity", a.Name, "credential", c.Name, "type", c.Type, "err", err.Error())
					continue
				}
				if name != "" && name == o.GetName() {
					out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(a)})
					break
				}
			}
		}
		return out
	}

	// The Secret watch fires only for Secrets the operator has ADOPTED: the
	// manager's Secret informer is label-filtered on adoptguard.AdoptedLabel, so
	// an unlabelled Secret produces no event here at all. It covers rotations of
	// an already-adopted Secret; the arrival of an unadopted one is covered by
	// secretArrivalRequeue instead, because the label this watch needs is only
	// ever stamped by the reconcile the watch would have triggered.
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AgentIdentity{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapSecretToAgents),
		).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var a spiceboxv1alpha1.AgentIdentity
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &a); !cont {
		return ctrl.Result{}, err
	}
	if a.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Snapshot pre-mutation status for MergeFrom patching. Sibling refresh
	// reconciler patches non-overlapping status fields concurrently; using
	// Update would 409-loop under Secret-watch retrigger.
	//
	// Taken BEFORE the platform link below, which stamps its own condition, and
	// BEFORE the revocation observation, which stamps status.observedCredentials:
	// a reconcile whose ONLY change is one of those must still produce a patch,
	// and a snapshot taken afterwards would compare equal and silently drop it.
	prior := a.DeepCopy()

	// Observe the credential set for the revocation diff, and stamp the durable
	// trigger state (status.observedCredentials) the status patch below
	// persists. Called after a successful load so the publisher primes on the
	// identity's first-ever reconcile and emits on subsequent credential
	// removals or replacements.
	//
	// Its error is CARRIED like linkErr, not returned here: a revoke that
	// failed to publish must not stop this identity's status from converging.
	// The publisher has already held the failed entries at their previous value
	// in both its cache and the stamped record, so returning the error after the
	// status write is what turns "held back" into "retried" — the requeue is the
	// only thing that re-runs the diff.
	var revokeErr error
	if r.RevokePublisher != nil {
		revokeErr = r.RevokePublisher.Observe(ctx, &a)
	}

	// Link this identity to the singleton platform object BEFORE every
	// validation gate below. agentidentity#update_credential is
	// `editor + platform->can_admin` with editor deliberately unpopulated, so
	// this tuple is the only thing that lets an admin's "replace this dead
	// credential" check resolve — and the identities that need it most are
	// exactly the ones that short-circuit below with a missing / empty /
	// expired Secret. Gating the write behind Valid=True would make the
	// permission unsatisfiable precisely when a human is being asked to fix
	// the credential.
	//
	// Its error is CARRIED, not returned here. Returning it before the status
	// write below would put SpiceDB into the hard path of a controller that
	// previously had none: while SpiceDB is unreachable, an AgentIdentity would
	// never get Valid=True/False at all, so every AgentClass waiting on it
	// stalls — a blast radius far larger than the one permission the link
	// governs. Status converges; the link error is still returned afterwards,
	// so the requeue-with-backoff and the loud log are both preserved.
	linkErr := r.ensurePlatformLink(ctx, &a)

	res, err := r.reconcileStatus(ctx, &a, prior)
	if err != nil {
		// A status-write failure outranks the link: it is the thing that
		// actually needs retrying first, and returning it keeps the linkErr's
		// own retry alive too (the next reconcile re-runs both).
		return res, err
	}
	if linkErr != nil {
		return ctrl.Result{}, linkErr
	}
	if revokeErr != nil {
		return ctrl.Result{}, revokeErr
	}
	return res, nil
}

// reconcileStatus is everything after the platform link: spec validation,
// Secret resolution, and the single status patch. Split out so Reconcile can
// run it even when the link write failed — see the linkErr comment there.
//
// prior is the caller's pre-mutation snapshot, threaded in rather than taken
// here so the link's condition is part of the diff.
func (r *Reconciler) reconcileStatus(ctx context.Context, a *spiceboxv1alpha1.AgentIdentity,
	prior *spiceboxv1alpha1.AgentIdentity) (ctrl.Result, error) {
	// Step 2 of the design: spec-shape validation, no API calls.
	if reason, msg := validateSpecShape(&a.Spec); reason != "" {
		return r.setInvalid(ctx, a, prior, reason, msg)
	}

	// Step 3: resolve every credentials[*].secretRef. Existence + key presence
	// only — never read or store the value bytes. Each Secret is adopted before
	// being read so the adoptguard SecretReader permits the access.
	//
	// A type=directCredentials/onepassword credential additionally has its
	// backing token VERIFIED against the declared vault here (set once per
	// token); priorPins threads the last-committed pins in so an unchanged token
	// carries its pin forward without re-hitting the backend. See
	// checkCredentialSecret / verifyDirectCredential.
	ownerRef := types.NamespacedName{Namespace: a.Namespace, Name: a.Name}
	resolved := make(map[string]bool, len(a.Spec.Credentials))
	priorPins := indexPinnedVaults(prior.Status.PinnedVaults)
	var pins []spiceboxv1alpha1.PinnedVault
	for _, c := range a.Spec.Credentials {
		pin, err := r.checkCredentialSecret(ctx, a.Namespace, ownerRef, c, priorPins[c.Name])
		if err != nil {
			switch {
			case errors.Is(err, credresolve.ErrSecretMissing):
				return r.setInvalidAndRetry(ctx, a, prior, spiceboxv1alpha1.ReasonSecretMissing,
					fmt.Sprintf("credentials[%s]: %v", c.Name, err), secretArrivalRequeue)
			case errors.Is(err, credresolve.ErrSecretKeyMissing):
				return r.setInvalid(ctx, a, prior, spiceboxv1alpha1.ReasonSecretKeyMissing, fmt.Sprintf("credentials[%s]: %v", c.Name, err))
			case errors.Is(err, credresolve.ErrSecretValueEmpty):
				return r.setInvalid(ctx, a, prior, spiceboxv1alpha1.ReasonCredentialEmpty, fmt.Sprintf("credentials[%s]: %v", c.Name, err))
			case errors.Is(err, credresolve.ErrExpired):
				return r.setInvalid(ctx, a, prior, spiceboxv1alpha1.ReasonAgentCredentialExpired, fmt.Sprintf("credentials[%s]: %v", c.Name, err))
			case errors.Is(err, errCredentialShapeMismatch):
				return r.setInvalid(ctx, a, prior, spiceboxv1alpha1.ReasonCredentialShapeMismatch, fmt.Sprintf("credentials[%s]: %v", c.Name, err))
			case errors.Is(err, errDirectCredentialInvalid):
				// A verify failure leaves a.Status.PinnedVaults untouched (the
				// local `pins` slice is only committed in step 4, which this
				// early return never reaches), so an existing pin is PRESERVED
				// rather than erased when a rotated token stops verifying.
				return r.setInvalid(ctx, a, prior, spiceboxv1alpha1.ReasonAgentIdentityDirectCredentialInvalid, fmt.Sprintf("credentials[%s]: %v", c.Name, err))
			default:
				return ctrl.Result{}, err // transient (API error) — requeue
			}
		}
		resolved[c.Name] = true
		if pin != nil {
			pins = append(pins, *pin)
		}
	}

	// Step 4: compute status.
	a.Status.ObservedGeneration = a.Generation
	a.Status.ResolvedCredentials = a.Status.ResolvedCredentials[:0]
	for _, c := range a.Spec.Credentials {
		if resolved[c.Name] {
			a.Status.ResolvedCredentials = append(a.Status.ResolvedCredentials, c.Name)
		}
	}
	// Sorted by credential name so an unchanged pin set re-writes byte-identically
	// (the DeepEqual below then drops the no-op patch). nil when no credential pins.
	slices.SortFunc(pins, func(x, y spiceboxv1alpha1.PinnedVault) int {
		return strings.Compare(x.Credential, y.Credential)
	})
	a.Status.PinnedVaults = pins
	conditions.SetTrue(a, &a.Status.Conditions,
		spiceboxv1alpha1.AgentIdentityConditionValid, spiceboxv1alpha1.ReasonAllReferencesResolve)
	if equality.Semantic.DeepEqual(prior.Status, a.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Client.Status().Patch(ctx, a, client.MergeFrom(prior))
}

// ensurePlatformLink writes the agentidentity#platform tuple through the
// injected linker.
//
// A nil linker (test fixtures / local dev without SpiceDB) is logged at INFO,
// not silently skipped: with the tuple absent every update_credential check
// fails closed and nothing else in the system says so. In production the
// operator always wires it, so the line only appears where it is a genuine
// misconfiguration.
//
// The outcome is recorded on AgentIdentityConditionPlatformLinked either way,
// so an operator can read with `kubectl` whether the permission that repairs
// this identity is satisfiable at all — the failure is otherwise entirely
// silent everywhere else in the system.
//
// A transient write failure is RETURNED (requeue with backoff), but the caller
// runs the status reconcile first — see Reconcile. An UNREPRESENTABLE name is
// returned as nil: no retry can ever succeed, so spinning on it forever would
// burn the workqueue and drown the log while telling nobody anything the
// condition does not already say.
func (r *Reconciler) ensurePlatformLink(ctx context.Context, a *spiceboxv1alpha1.AgentIdentity) error {
	if r.PlatformLinker == nil {
		log.FromContext(ctx).Info("PlatformLinker not configured; skipping the agentidentity#platform link — agentidentity#update_credential is UNSATISFIABLE for this identity, so no platform admin can replace its credentials",
			"agentIdentity", a.Namespace+"/"+a.Name)
		// The condition is deliberately NOT stamped here. A nil linker is a
		// binary-wiring fact (local dev, a test fixture), identical for every
		// AgentIdentity in the process; writing PlatformLinked=False onto every
		// CR would be status noise about the operator, not about the object.
		return nil
	}
	err := r.PlatformLinker.EnsureAgentIdentityPlatform(ctx, a.Namespace, a.Name)
	switch {
	case err == nil:
		conditions.SetTrue(a, &a.Status.Conditions,
			spiceboxv1alpha1.AgentIdentityConditionPlatformLinked, spiceboxv1alpha1.ReasonPlatformLinked)
		return nil

	case errors.Is(err, spicedb.ErrUnrepresentableObjectID):
		// PERMANENT, and unfixable without recreating the CR under another
		// name (a Kubernetes name is immutable). Retrying is pointless, so the
		// condition IS the report — this is the one link failure a human must
		// act on rather than wait out.
		log.FromContext(ctx).Info("this AgentIdentity's name cannot be expressed as a SpiceDB object id, so the agentidentity#platform link can NEVER be written and agentidentity#update_credential is permanently unsatisfiable for it; no platform admin will be able to replace its credentials. Recreate it under a name without '.' (or any character outside [a-zA-Z0-9/_|-=+]). NOT retrying",
			"agentIdentity", a.Namespace+"/"+a.Name, "err", err.Error())
		conditions.SetFalse(a, &a.Status.Conditions,
			spiceboxv1alpha1.AgentIdentityConditionPlatformLinked,
			spiceboxv1alpha1.ReasonUnrepresentableIdentityName, err.Error())
		return nil

	default:
		// Most failures here are transient (SpiceDB unreachable) and the requeue
		// clears them. One is NOT: "object definition `agentidentity` not found"
		// means the live SpiceDB schema predates this operator, and no amount of
		// retrying this write fixes it — the guardian controller's RunAll has to
		// compose and write the schema first. That self-heals at operator start
		// on any cluster with an AgentClass, so it is not worth a distinct code
		// path, but it IS worth naming: the two look identical in the logs and
		// the permanent one otherwise reads as a stuck retry loop.
		log.FromContext(ctx).Info("writing the agentidentity#platform link failed; update_credential stays unsatisfiable for this identity until it succeeds — requeueing. If the error is \"object definition `agentidentity` not found\", this is PERMANENT until the guardian controller writes the composed schema, not a transient SpiceDB blip",
			"agentIdentity", a.Namespace+"/"+a.Name, "err", err.Error())
		conditions.SetFalse(a, &a.Status.Conditions,
			spiceboxv1alpha1.AgentIdentityConditionPlatformLinked,
			spiceboxv1alpha1.ReasonPlatformLinkFailed, err.Error())
		return fmt.Errorf("ensure agentidentity#platform link: %w", err)
	}
}

// checkCredentialSecret adopts the credential's referenced Secret and reads it
// via SecretReader, then validates existence, key presence, and (for oauth)
// expiry. Returns credresolve sentinel errors so callers can map to conditions.
//
// For a type=directCredentials credential it additionally proves, via the
// directcred source's Verify, which vault the token resolves to, returning the
// PinnedVault to record on status — set once per token (priorPin threads the
// last-committed pin in so an unchanged token skips the backend call). A nil
// pin means "nothing to pin" (not a directCredentials credential, or a source
// with no vault concept); a non-nil pin is appended to status.pinnedVaults by
// the caller. errDirectCredentialInvalid is returned when Verify fails — or
// when the named source is not even registered (fail closed; see
// verifyDirectCredential).
func (r *Reconciler) checkCredentialSecret(ctx context.Context, ns string, ownerRef types.NamespacedName,
	cred spiceboxv1alpha1.AgentCredential, priorPin *spiceboxv1alpha1.PinnedVault) (*spiceboxv1alpha1.PinnedVault, error) {
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return nil, err
	}
	ref := k.SecretRef(cred)
	if ref == nil {
		return nil, nil // nothing stored; nothing to adopt or key-check
	}
	secretName := ref.Name

	secretRef := types.NamespacedName{Namespace: ns, Name: secretName}
	// Adopt the referenced Secret — metadata-only SSA that stamps AdoptedLabel
	// so subsequent reads via SecretReader are permitted. Adopt's existence check
	// uses the live reader (r.SecretReader.Reader), so a not-yet-adopted Secret
	// (absent from the label-filtered cache) is seen. A NotFound from adopt drives
	// the user-visible SecretMissing error — the same signal the dropped cached
	// pre-check produced — without silently minting an empty Secret via the SSA apply.
	if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "AgentIdentity"); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s", credresolve.ErrSecretMissing, ns, secretName)
		}
		return nil, fmt.Errorf("%w: %s/%s (adopt: %v)", credresolve.ErrSecretMissing, ns, secretName, err)
	}
	sec, err := r.SecretReader.Get(ctx, secretRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s", credresolve.ErrSecretMissing, ns, secretName)
		}
		return nil, err
	}

	// Expiry is a fact about whether a STORED value is still usable, not
	// about the Secret's shape, so it is gated on NeedsRefresh() — the fact
	// that actually means "this Secret holds a value that expires and is
	// refreshed out of band" — rather than folded into the key loop below.
	if k.NeedsRefresh() {
		if exp, ok := sec.Data["expires_at"]; ok && len(exp) > 0 {
			t, perr := time.Parse(time.RFC3339, string(exp))
			if perr == nil && !t.After(time.Now()) {
				return nil, fmt.Errorf("%w: %s/%s", credresolve.ErrExpired, ns, secretName)
			}
		}
	}

	// Every key this type's Secret shape requires — static's single named
	// key, oauth's access_token, or a minted multi-key kind's own fields (a
	// GitHub App's app-id/private-key/installation-id) — is checked
	// generically here, from the kind itself, instead of a hand-rolled
	// per-type case. That is what closes the gap a type this checker has
	// never heard of used to fall through with no validation at all.
	//
	// keyErr preserves oauth's own historical message shape: unquoted
	// "key=access_token" (NeedsRefresh() is oauth's own fact today), versus
	// the quoted "key=%q" every other type's key check has always used.
	keyErr := func(sentinel error, key string) error {
		if k.NeedsRefresh() {
			return fmt.Errorf("%w: %s/%s key=%s", sentinel, ns, secretName, key)
		}
		return fmt.Errorf("%w: %s/%s key=%q", sentinel, ns, secretName, key)
	}
	for _, key := range k.RequiredSecretKeys(cred) {
		val, ok := sec.Data[key]
		if !ok {
			return nil, keyErr(credresolve.ErrSecretKeyMissing, key)
		}
		if len(val) == 0 {
			return nil, keyErr(credresolve.ErrSecretValueEmpty, key)
		}
	}

	if err := r.checkCredentialShape(ctx, ns, k, cred); err != nil {
		return nil, err
	}
	// Last, because it is the only step that reaches an external backend: prove
	// the token's vault for a type=directCredentials credential (no-op for every
	// other type). ref carries the Secret key holding the token value.
	return r.verifyDirectCredential(ctx, cred, ref, sec.Data, priorPin)
}

// verifyDirectCredential proves, for a type=directCredentials credential, which
// vault its backing token resolves to, returning the PinnedVault to record.
//
//   - A non-directCredentials credential returns (nil, nil): nothing to verify.
//   - The pin is SET ONCE per token: an unchanged TokenFingerprint carries
//     priorPin forward WITHOUT calling the backend, so a steady-state reconcile
//     makes no external call and writes nothing new.
//   - On a changed/first token it calls the source's Verify. A verify error maps
//     to errDirectCredentialInvalid (its text names vaults/counts, never the
//     token — every directcred source scrubs it). A source with no vault concept
//     (secret) returns a zero Pin and pins nothing.
//   - An UNREGISTERED source FAILS CLOSED, mirroring credkindregistry.Get: the
//     identity must not report Valid=True while the vault-pin gate silently
//     cannot run — the pin is the whole defense against a rotated token
//     quietly resolving to a different vault, and "healthy but unguarded" is
//     the exact failure it exists to prevent. The CRD enum admits only known
//     source names, so this branch is reachable only in the
//     enum-updated-without-blank-import window (the same window
//     TestEveryCRDEnumValueHasARegisteredKind guards for credkinds; the
//     directcred twin lives in pkg/platform/identity/directcred's
//     enum_invariant_test.go).
func (r *Reconciler) verifyDirectCredential(ctx context.Context, cred spiceboxv1alpha1.AgentCredential,
	ref *credkind.SecretRef, secretData map[string][]byte, priorPin *spiceboxv1alpha1.PinnedVault) (*spiceboxv1alpha1.PinnedVault, error) {
	dc := cred.DirectCredentials
	if dc == nil {
		return nil, nil
	}
	src, ok := directcred.Get(dc.Source)
	if !ok {
		return nil, fmt.Errorf(
			"%w: no directcred source registered for source kind %q (registered: %s); the binary may be missing its blank import of the source's package under pkg/platform/identity/directcred/",
			errDirectCredentialInvalid, dc.Source, registeredDirectCredKinds())
	}

	// ref.Key names the Secret key holding the token the source authenticates
	// with ("token" for onepassword; "" for secret, which has no single token
	// and never pins). The fingerprint is a digest, never the value.
	fprint := tokenFingerprint(secretData[ref.Key])
	if priorPin != nil && priorPin.TokenFingerprint == fprint {
		carried := *priorPin
		return &carried, nil
	}

	pin, err := src.Verify(ctx, directcred.Config{SecretData: secretData, Vault: dc.Vault})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errDirectCredentialInvalid, err)
	}
	if pin.VaultID == "" {
		return nil, nil // source has no vault concept (secret): nothing to pin
	}
	return &spiceboxv1alpha1.PinnedVault{
		Credential:       cred.Name,
		VaultID:          pin.VaultID,
		VaultName:        pin.VaultName,
		TokenFingerprint: fprint,
	}, nil
}

// errDirectCredentialInvalid marks a type=directCredentials credential whose
// token could not be proven to resolve to its declared vault. Package-local: it
// carries the verdict from verifyDirectCredential up to the one switch that maps
// it onto ReasonAgentIdentityDirectCredentialInvalid.
var errDirectCredentialInvalid = errors.New("direct credential vault verification failed")

// registeredDirectCredKinds names every registered directcred source kind for
// the fail-closed error above — the same "(registered: ...)" style
// credkindregistry.Get uses, so the two registries' wiring failures read alike.
func registeredDirectCredKinds() string {
	all := directcred.All()
	if len(all) == 0 {
		return "none"
	}
	kinds := make([]string, len(all))
	for i, s := range all {
		kinds[i] = s.Kind()
	}
	return strings.Join(kinds, ", ")
}

// tokenFingerprint is a hex sha256 digest of a token value — the set-once key
// for a vault pin. Same three-line shape (and reasoning) as the per-kind
// tokenFingerprint helpers under pkg/channels/channelkinds/*: a small local
// helper over a speculative shared extraction. Never stores or compares the
// token itself.
func tokenFingerprint(tok []byte) string {
	sum := sha256.Sum256(tok)
	return hex.EncodeToString(sum[:])
}

// indexPinnedVaults keys the committed pins by credential name so a reconcile
// can find the prior pin for a credential in O(1). The pointers alias the
// caller's slice (the pre-mutation snapshot), so callers must treat them as
// read-only.
func indexPinnedVaults(pins []spiceboxv1alpha1.PinnedVault) map[string]*spiceboxv1alpha1.PinnedVault {
	if len(pins) == 0 {
		return nil
	}
	m := make(map[string]*spiceboxv1alpha1.PinnedVault, len(pins))
	for i := range pins {
		m[pins[i].Credential] = &pins[i]
	}
	return m
}

// errCredentialShapeMismatch marks a credential whose stored value does not
// match the token format its provider declares. Package-local: it exists to
// carry the verdict from checkCredentialSecret up to the one switch that maps
// it onto a condition reason.
var errCredentialShapeMismatch = errors.New("credential value has the wrong shape")

// checkCredentialShape validates the credential's STORED VALUE against the
// token format its provider declares.
//
// The provider catalog already declares each credential's shape
// (provider.TokenShape) and every INTERACTIVE entry point already enforces it —
// the identityd paste forms, `oap identity put-token`, the builtin setup flows.
// A Secret written out of band (`kubectl create secret generic ...`) passes
// through none of them, and this is the point at which the operator first sees
// such a Secret. Without the check the wrong credential reports healthy
// everywhere and fails minutes later as an opaque 401 inside a tool's own
// output.
//
// It is generic in both directions. Which shape to expect comes from the
// catalog — passthroughcatalog.ProviderForCredential resolves the credential
// name through a toolkit's env binding or an MCPServer's spec.auth — and which
// value to check comes from the credkind's own ReadStoredValue, so no
// credential type or provider is named here.
//
// It stays PERMISSIVE wherever the expected shape is unknown: a minted kind
// stores no value to check, an unresolvable credential name declares no
// provider, and a provider may declare no format. None of those is a reason to
// refuse a credential — the same rule provider.ValidateToken itself follows.
func (r *Reconciler) checkCredentialShape(ctx context.Context, ns string,
	k credkind.Kind, cred spiceboxv1alpha1.AgentCredential) error {
	if k.Minted() {
		return nil // produced fresh per resolve; there is no stored value to inspect
	}
	p, ok := passthroughcatalog.ProviderForCredential(ctx, r.Client, cred.Name)
	if !ok || p.TokenShape == nil || p.TokenShape.Pattern == "" {
		return nil
	}
	// Re-reads the Secret rather than reusing the one already in hand, because
	// which of its keys holds the credential's value is the KIND's answer, not
	// this reconciler's — the same dispatch credresolve.ResolveSecretValue makes.
	// The read is live, already-adopted, and off any hot path.
	val, err := k.ReadStoredValue(ctx, r.SecretReader.Reader, ns, cred)
	if err != nil {
		return err
	}
	if verr := provider.ValidateToken(*p, string(val.UnderlyingValue())); verr != nil {
		// verr names the expected shape and any shape the value DOES match; it
		// never contains the value itself.
		return fmt.Errorf("%w: %v", errCredentialShapeMismatch, verr)
	}
	return nil
}

// setInvalid stamps Valid=False and patches. prior is the caller's snapshot,
// taken before the platform link stamped its own condition, so a reconcile in
// which only the link outcome changed still patches.
func (r *Reconciler) setInvalid(ctx context.Context, a, prior *spiceboxv1alpha1.AgentIdentity,
	reason, msg string) (ctrl.Result, error) {
	a.Status.ResolvedCredentials = nil
	return apreconcile.SetInvalid(ctx, a, &a.Status.ObservedGeneration, &a.Status.Conditions,
		spiceboxv1alpha1.AgentIdentityConditionValid, reason, msg,
		func(ctx context.Context) error { return r.Client.Status().Patch(ctx, a, client.MergeFrom(prior)) })
}

// setInvalidAndRetry is setInvalid plus a timed re-check. It is for the
// invalid states this reconciler will NOT be told about — see
// secretArrivalRequeue.
//
// A failed status patch still outranks the requeue: the returned error carries
// its own backoff, and stamping a RequeueAfter alongside it would replace that
// backoff with a fixed delay.
func (r *Reconciler) setInvalidAndRetry(ctx context.Context, a, prior *spiceboxv1alpha1.AgentIdentity,
	reason, msg string, after time.Duration) (ctrl.Result, error) {
	res, err := r.setInvalid(ctx, a, prior, reason, msg)
	if err != nil {
		return res, err
	}
	res.RequeueAfter = after
	return res, nil
}

// validateSpecShape returns a non-empty (reason, message) when the spec is
// shape-invalid. All checks are deterministic and do not hit the API server.
func validateSpecShape(s *spiceboxv1alpha1.AgentIdentitySpec) (string, string) {
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
		// this reconciler and UserIdentity's alike. See
		// credkindregistry.ValidateCredential.
		if err := credkindregistry.ValidateCredential(c, credkind.ScopeAgentIdentity); err != nil {
			return spiceboxv1alpha1.ReasonSpecInvalid, err.Error()
		}
	}

	return "", ""
}
