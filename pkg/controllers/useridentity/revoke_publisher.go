// RevokePublisher is the operator-side detective control pairing with the
// runner's in-process broker cache: on every UserIdentity reconcile it diffs
// Spec.Credentials against the per-credential fingerprint from the previous
// observation and emits a revocation on the unified ap.revocation bus per
// change.
//
//   - Name present in previous, absent in current: REMOVED → emit a
//     "credential" revoke, logged reason "revoked".
//   - Same name, different fingerprint: REPLACED → emit a "credential" revoke,
//     logged reason "refreshed"; covers an OAuth refresh producing a new Secret
//     ref and operator-driven static→oauth migrations.
//   - New name appears: ADDED → no emit. KindCredentialLinked covers that
//     direction, via channelsd's CredentialLinkedWatcher.
//
// The revoke key is "<secretNamespace>/<secretName>", the shape the credential
// Invalidator parses to drop the broker's cached token. The reason is computed
// for the operator log line only — the unified RevokedPayload has no reason
// field.
//
// # Where the trigger state lives
//
// The previous observation is a DURABLE record on the object,
// UserIdentity.status.observedCredentials, with the in-process map in front of
// it only as a cache. That ordering is the whole point: NATS delivery is
// at-most-once with no fallback path for revocations, so a process-scoped map
// is the wrong place for the only copy of "this credential still owes a
// revoke". The durable record closes two failure modes:
//
//   - An unlink committed while the operator was DOWN. The successor process
//     has an empty map, seeds from status, and re-derives the diff instead of
//     priming past it.
//   - A publish that FAILED. The failed entry keeps its PREVIOUS value in both
//     cache and record, so the next reconcile re-derives the same diff and
//     re-emits. Observe reports the failure so the reconciler can requeue
//     rather than wait for the informer resync.
//
// Emit happens BEFORE the record advances, deliberately: a lost status write
// costs a duplicate revoke next reconcile, and dropping an already-gone cache
// entry is a no-op, whereas advancing the record first loses the revocation
// entirely.
//
// An identity with neither a durable record nor a cache entry is a first
// observation: prime, no emit.
//
// Concurrent-safe; share one instance across the controller's reconciles.
package useridentity

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	iduseridentity "github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// credentialRevokeKind is the registered revocation.Invalidator kind the
// emitted revokes target — matches credential.Invalidator.Kind().
const credentialRevokeKind = "credential"

// RevokePublisher diffs UserIdentity.Spec.Credentials against the last
// observation and emits credential revokes on the unified ap.revocation bus for
// removed or replaced credentials. Mirrors channelsd's CredentialLinkedWatcher
// in the opposite direction, driven by Reconcile rather than a polling loop.
//
// Concurrent-safe; share one instance across the controller's reconciles.
type RevokePublisher struct {
	// Bus delivers revocations on the unified ap.revocation subject. Nil is
	// tolerated for an operator running without NATS — Observe still maintains
	// the diff state but skips publishing — since Emit no-ops on both a
	// nil-backed publisher and a nil receiver, and emit() logs it.
	Bus *revocation.Publisher

	mu sync.Mutex
	// observed caches the last-seen credential set, keyed uiName → credName →
	// fingerprint; UserIdentity is cluster-scoped, so the object name is a
	// complete key. A CACHE in front of status.observedCredentials, never the
	// source of truth: a cold entry is seeded from the durable record.
	observed map[string]map[string]string
}

// NewRevokePublisher constructs the publisher. A nil bus is tolerated where the
// operator runs without NATS: Observe still maintains the diff state, keeping
// behavior deterministic, but publishes no envelopes.
func NewRevokePublisher(bus *revocation.Publisher) *RevokePublisher {
	return &RevokePublisher{
		Bus:      bus,
		observed: map[string]map[string]string{},
	}
}

// Observe is called from the controller's Reconcile after a successful
// UserIdentity load. It diffs the new credential set against the last
// observation for this UserIdentity and emits a credential revoke for
// each REMOVED (reason="revoked") or REPLACED (reason="refreshed")
// entry on the unified ap.revocation bus.
//
// It then stamps the advanced record onto ui.Status.ObservedCredentials.
// The CALLER's status write is what persists it, so Observe must run after
// the caller has snapshotted status for its patch — otherwise the stamp is
// inside the snapshot and the patch drops it.
//
// The first observation of a UserIdentity — no cache entry and no durable
// record — primes without emitting.
//
// ADDED credentials are intentionally NOT emitted here: KindCredentialLinked,
// via the channelsd-side watcher, covers the user-visible "your credential is
// linked" path.
//
// Returns a non-nil error when at least one revoke failed to publish. Each
// failure is also logged; the error exists so the reconciler can requeue,
// because the entries that failed are deliberately held at their previous
// value and only another reconcile can retry them.
func (p *RevokePublisher) Observe(ctx context.Context, ui *spiceboxv1alpha1.UserIdentity) error {
	logger := log.FromContext(ctx).WithName("revoke-publisher").WithValues("useridentity", ui.Name)
	p.mu.Lock()
	defer p.mu.Unlock()

	current := make(map[string]string, len(ui.Spec.Credentials))
	for _, c := range ui.Spec.Credentials {
		if c.Name == "" {
			// Defensive: malformed credential entry; the controller's
			// spec-shape check would have already set Valid=False.
			// Skip rather than crash.
			continue
		}
		current[c.Name] = credentialFingerprint(logger, c)
	}

	prev, hadPrev := p.observed[ui.Name]
	if !hadPrev {
		// Cold cache — a fresh process, or the first reconcile of this
		// identity. The durable record is the only thing that can say
		// whether a credential disappeared while nobody was watching.
		if durable := fingerprintsFromStatus(ui.Status.ObservedCredentials); len(durable) > 0 {
			prev, hadPrev = durable, true
			logger.V(1).Info("seeded observed state from status.observedCredentials",
				"credentialCount", len(durable))
		}
	}
	if !hadPrev {
		p.observed[ui.Name] = current
		ui.Status.ObservedCredentials = observedCredentialsForStatus(current)
		logger.V(1).Info("prime observed state; no emit",
			"credentialCount", len(current))
		return nil
	}

	// heldBack records, per credential name, the PREVIOUS fingerprint that
	// must stay in the observed record because its revoke did not publish.
	// Both the cache and status keep that value, so the next reconcile
	// re-derives the identical diff and retries.
	heldBack := map[string]string{}
	var errs []error

	// REMOVED: name in prev but not in current.
	for name, fp := range prev {
		if _, stillThere := current[name]; stillThere {
			continue
		}
		if err := p.emit(ctx, logger, ui.Name, ui.Spec.Subject, name, "revoked"); err != nil {
			heldBack[name] = fp
			errs = append(errs, err)
		}
	}
	// REPLACED: name in both, different fingerprint.
	for name, fp := range current {
		prevFP, ok := prev[name]
		if !ok || prevFP == fp {
			continue
		}
		if err := p.emit(ctx, logger, ui.Name, ui.Spec.Subject, name, "refreshed"); err != nil {
			heldBack[name] = prevFP
			errs = append(errs, err)
		}
	}
	// ADDED (name in current, absent in prev) is intentionally NOT
	// emitted — covered by KindCredentialLinked on the channelsd side.

	next := advance(current, heldBack)
	p.observed[ui.Name] = next
	ui.Status.ObservedCredentials = observedCredentialsForStatus(next)
	return errors.Join(errs...)
}

// advance builds the credential set to record: the current spec, with every
// entry whose revoke failed to publish rolled back to the fingerprint it had
// before — including entries the spec no longer mentions, which must stay in
// the record or the removal would be forgotten.
func advance(current, heldBack map[string]string) map[string]string {
	next := make(map[string]string, len(current)+len(heldBack))
	for name, fp := range current {
		next[name] = fp
	}
	for name, fp := range heldBack {
		next[name] = fp
	}
	return next
}

// emit publishes one credential revoke on the unified ap.revocation bus.
// The revoke key is "<secretNamespace>/<secretName>" computed from the
// backing master Secret coordinates — the same shape the credential
// Invalidator parses to drop the broker's cached token. Errors are
// logged (per the "never silently drop errors" AGENTS.md rule) AND returned,
// so the caller can hold the trigger state back and requeue.
//
// The reason ("revoked"/"refreshed") no longer rides the envelope (the
// unified RevokedPayload has no reason field); it is kept solely for the
// operator log line so the why-was-this-revoked context survives.
func (p *RevokePublisher) emit(ctx context.Context, logger logr.Logger, uiName, subject, credName, reason string) error {
	if p.Bus == nil {
		// Local-dev / unit-test path: still log so an operator running
		// without NATS can see the diff was detected. Not an error — there is
		// nothing to retry, so holding the trigger state back would re-log
		// this line on every reconcile forever.
		logger.Info("revoke publisher: bus nil; skipping publish",
			"subject", subject, "cred", credName, "reason", reason)
		return nil
	}
	// Compute the backing master-Secret coordinates. The broker keys its
	// cache by (namespace, Secret name), so the credential Invalidator
	// needs the physical Secret, not the catalog name. UserIdentity
	// credentials always live in IdentitiesNamespace under
	// MasterSecretName(uiName, credName).
	secretNs := spiceboxv1alpha1.IdentitiesNamespace
	secretName := iduseridentity.MasterSecretName(uiName, credName)
	key := secretNs + "/" + secretName

	// Credential revokes are emitted CLUSTER-WIDE (scope ""): the runner-side
	// subscriber registers with the session's namespace, which for a
	// userPassthrough session is rarely IdentitiesNamespace, so a scope-gated
	// emit would be dropped by revocation.Applies before invalidation. The key
	// is "<secretNamespace>/<secretName>" — fully namespace-qualified and
	// globally unique — so broker.InvalidateSecret only ever drops the one
	// matching cache entry regardless of which runner receives it (a miss is a
	// safe no-op). The scope gate exists only for tool-origin revokes, whose
	// keys aren't namespace-qualified.
	if err := p.Bus.Emit(ctx, credentialRevokeKind, key, ""); err != nil {
		logger.Info("revoke publisher: emit failed; holding the trigger state back so the next reconcile retries",
			"err", err.Error(),
			"subject", subject, "cred", credName, "reason", reason, "key", key)
		return err
	}
	logger.Info("credential change published",
		"subject", subject, "cred", credName, "reason", reason, "key", key)
	return nil
}

// fingerprintsFromStatus rebuilds the in-memory fingerprint map from the
// durable record. An entry with an empty name is dropped: it can only come
// from a hand-edited status and would otherwise shadow a real credential.
func fingerprintsFromStatus(recs []spiceboxv1alpha1.ObservedCredential) map[string]string {
	out := make(map[string]string, len(recs))
	for _, r := range recs {
		if r.Name == "" {
			continue
		}
		out[r.Name] = r.Fingerprint
	}
	return out
}

// observedCredentialsForStatus renders the observed set for
// status.observedCredentials, sorted by name so an unchanged credential set
// produces a byte-identical value and the status write stays a no-op. An
// empty set renders as nil, not [], so it round-trips through omitempty and
// compares equal to the zero value under apiequality.Semantic.
//
// SecretNames is left empty: a UserIdentity credential's master Secret name
// is derived from (identity, credential) by MasterSecretName, so a removed
// credential's revoke key needs nothing the record does not already carry.
func observedCredentialsForStatus(fps map[string]string) []spiceboxv1alpha1.ObservedCredential {
	if len(fps) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.ObservedCredential, 0, len(fps))
	for name, fp := range fps {
		out = append(out, spiceboxv1alpha1.ObservedCredential{Name: name, Fingerprint: fp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// credentialFingerprint computes "<type>:<secretRefName>" for one
// AgentCredential: stable across reconciles, sensitive to type changes
// (static↔oauth) and to Secret-ref renames, which is how an OAuth refresh
// surfaces a new credential in the multi-key Secret shape. Same scheme as
// channelsd's CredentialLinkedWatcher uses.
//
// Returns "<type>:" when the discriminator block is missing. That is a malformed
// AgentCredential, but returning rather than dereferencing nil is safe: the
// controller's spec-shape check has already set Valid=False, and this
// fingerprint compares unequal to any well-formed version, so the next valid
// update still triggers a REPLACED emit.
func credentialFingerprint(logger logr.Logger, c spiceboxv1alpha1.AgentCredential) string {
	k, err := credkindregistry.Get(c.Type)
	if err != nil {
		// Unregistered type: logged per the no-silent-errors rule. The revoke
		// itself still fires on removal/replacement (emit derives the Secret
		// coordinates from MasterSecretName, not from this fingerprint), but a
		// type nothing can resolve is a config problem worth surfacing.
		logger.Info("revoke publisher: credential has an unregistered type",
			"credential", c.Name, "type", c.Type, "err", err.Error())
		return c.Type + ":"
	}
	ref := k.SecretRef(c)
	if ref == nil || ref.Name == "" {
		return c.Type + ":"
	}
	return c.Type + ":" + ref.Name
}
