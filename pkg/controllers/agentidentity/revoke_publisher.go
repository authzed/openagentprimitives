// pkg/controllers/agentidentity/revoke_publisher.go
//
// RevokePublisher is the operator-side detective control that pairs with
// the runner's in-process broker cache: on every AgentIdentity reconcile,
// it diffs Spec.Credentials against the per-credential fingerprint from
// the previous observation and emits a revocation on the unified
// ap.revocation bus (revocation.Publisher) per change.
//
//   - Credential name present in previous, absent in current: REMOVED
//     → emit a "credential" revoke (reason="revoked" for the log line).
//   - Same name, different fingerprint: REPLACED → emit a "credential"
//     revoke (reason="refreshed" for the log line; covers OAuth refresh
//     producing a new Secret ref, and operator-driven static→oauth
//     migrations).
//   - New credential name appears: ADDED → no emit. KindCredentialLinked
//     covers that direction.
//
// The emitted revoke key is "<secretNamespace>/<secretName>" — the same
// shape the credential Invalidator (pkg/authz/revocation/kinds/credential)
// parses to call broker.InvalidateSecret. For AgentIdentity credentials,
// the Secret namespace is the AgentIdentity's own namespace (not
// IdentitiesNamespace); this matches how the in-process broker keys its
// cache via RuntimeIdentityFromAgentIdentity → ai.Namespace.
//
// # Where the trigger state lives
//
// The previous observation is a DURABLE record on the object —
// AgentIdentity.status.observedCredentials — with the in-process map in
// front of it as a cache. That ordering is the whole point: NATS delivery
// is at-most-once and there is no fallback path for revocations, so a
// process-scoped map is the wrong place for the only copy of "this
// credential still owes a revoke". Two failure modes the durable record
// closes:
//
//   - A removal committed while the operator was DOWN. The successor
//     process has an empty map; it seeds from status and re-derives the
//     diff instead of priming past it.
//   - A publish that FAILED. The failed entry keeps its PREVIOUS value in
//     both the cache and the record, so the next reconcile re-derives the
//     same diff and re-emits. Observe reports the failure to the caller so
//     the reconciler can requeue rather than wait for the informer resync.
//
// Emit happens BEFORE the record advances, deliberately: a lost status
// write costs a duplicate revoke on the next reconcile (idempotent — the
// invalidator drops a cache entry that is already gone), whereas a record
// advanced ahead of the emit costs the revocation entirely.
//
// An identity with no durable record and no cache entry is a first
// observation: prime, no emit — anti-spam on operator restart, and the
// reason a fresh install does not revoke credentials nobody ever held.
//
// Concurrent-safe; one instance shared across the controller's reconciles
// via the Reconciler.RevokePublisher field.
package agentidentity

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
)

// credentialRevokeKind is the registered revocation.Invalidator kind the
// emitted revokes target — matches credential.Invalidator.Kind().
const credentialRevokeKind = "credential"

// credentialSnapshot is the per-credential observed state. It stores the
// fingerprint (for change detection) plus the backing Secret names (so a
// REMOVED credential can be revoked without hitting the API server again).
type credentialSnapshot struct {
	// fingerprint is "<type>:<secretRefName>" — stable across reconciles,
	// sensitive to type changes (static↔oauth) and Secret-ref renames.
	fingerprint string
	// secretNames are the backing Secret names for this credential.
	// Typically one (static → 1, oauth → 1), but defensively handles
	// malformed specs where both Static and OAuth are set.
	secretNames []string
}

// RevokePublisher diffs AgentIdentity.Spec.Credentials against the last
// observation and emits credential revokes on the unified ap.revocation bus
// for removed or replaced credentials. Mirrors the useridentity.RevokePublisher
// pattern but targets AgentIdentity-scoped credentials whose Secrets live
// in the AgentIdentity's own namespace.
//
// Concurrent-safe; share one instance across the controller's reconciles.
type RevokePublisher struct {
	// Bus delivers revocations on the unified ap.revocation subject; nil
	// is tolerated for local-dev flows where the operator is run without
	// NATS (Observe maintains the diff state but skips publishing).
	// revocation.Publisher.Emit already no-ops on a nil-backed publisher,
	// but a nil *revocation.Publisher itself is also tolerated (Emit has a
	// nil-receiver guard) and surfaced via a log line in emit().
	Bus *revocation.Publisher

	mu sync.Mutex
	// observed caches the last-seen credential set per AgentIdentity, keyed
	// "<namespace>/<name>" → credName → snapshot. It is a CACHE in front of
	// status.observedCredentials, not the source of truth: a cold entry is
	// seeded from the object's durable record. Keyed by namespace+name
	// because AgentIdentity is namespaced — two identities may legitimately
	// share a name, and a name-only key would let one namespace's prime
	// answer for the other's credentials.
	observed map[string]map[string]credentialSnapshot
}

// NewRevokePublisher constructs the publisher. A nil bus is tolerated for
// local-dev configurations where the operator runs without NATS; Observe
// will still maintain the diff state so behavior stays deterministic, but
// no envelopes are published.
func NewRevokePublisher(bus *revocation.Publisher) *RevokePublisher {
	return &RevokePublisher{
		Bus:      bus,
		observed: map[string]map[string]credentialSnapshot{},
	}
}

// Observe is called from the controller's Reconcile after a successful
// AgentIdentity load. It diffs the new credential set against the last
// observation for this AgentIdentity and emits a credential revoke for
// each REMOVED (reason="revoked") or REPLACED (reason="refreshed") entry
// on the unified ap.revocation bus.
//
// It then stamps the advanced record onto ai.Status.ObservedCredentials.
// The CALLER's status write is what persists it, so Observe must run after
// the caller has snapshotted status for its patch — otherwise the stamp is
// inside the snapshot and the patch drops it.
//
// The first observation of an AgentIdentity — no cache entry and no durable
// record — primes without emitting.
//
// ADDED credentials are intentionally NOT emitted here.
//
// Returns a non-nil error when at least one revoke failed to publish. Each
// failure is also logged; the error exists so the reconciler can requeue,
// because the entries that failed are deliberately held at their previous
// value and only another reconcile can retry them.
func (p *RevokePublisher) Observe(ctx context.Context, ai *spiceboxv1alpha1.AgentIdentity) error {
	logger := log.FromContext(ctx).WithName("revoke-publisher").WithValues("agentidentity", ai.Namespace+"/"+ai.Name)
	p.mu.Lock()
	defer p.mu.Unlock()

	current := make(map[string]credentialSnapshot, len(ai.Spec.Credentials))
	for _, c := range ai.Spec.Credentials {
		if c.Name == "" {
			// Defensive: malformed credential entry; the controller's
			// spec-shape check would have already set Valid=False.
			// Skip rather than crash.
			continue
		}
		current[c.Name] = agentCredentialSnapshot(logger, c)
	}

	key := ai.Namespace + "/" + ai.Name
	prev, hadPrev := p.observed[key]
	if !hadPrev {
		// Cold cache — a fresh process, or the first reconcile of this
		// identity. The durable record is the only thing that can say
		// whether a credential disappeared while nobody was watching.
		if durable := snapshotsFromStatus(ai.Status.ObservedCredentials); len(durable) > 0 {
			prev, hadPrev = durable, true
			logger.V(1).Info("seeded observed state from status.observedCredentials",
				"credentialCount", len(durable))
		}
	}
	if !hadPrev {
		p.observed[key] = current
		ai.Status.ObservedCredentials = observedCredentialsForStatus(current)
		logger.V(1).Info("prime observed state; no emit",
			"credentialCount", len(current))
		return nil
	}

	// heldBack records, per credential name, the PREVIOUS snapshot that must
	// stay in the observed record because its revoke did not publish. Both
	// the cache and status keep that value, so the next reconcile re-derives
	// the identical diff and retries.
	heldBack := map[string]credentialSnapshot{}
	var errs []error

	// REMOVED: name in prev but not in current — emit for each backing
	// Secret stored in the previous snapshot.
	for name, snap := range prev {
		if _, stillThere := current[name]; stillThere {
			continue
		}
		if err := p.emitAll(ctx, logger, ai.Namespace, snap.secretNames, name, "revoked"); err != nil {
			heldBack[name] = snap
			errs = append(errs, err)
		}
	}
	// REPLACED: name in both, different fingerprint — emit for each backing
	// Secret in the NEW snapshot (the old token is invalid, new one will be
	// fetched fresh on next Resolve).
	for name, snap := range current {
		prevSnap, ok := prev[name]
		if !ok || prevSnap.fingerprint == snap.fingerprint {
			continue
		}
		if err := p.emitAll(ctx, logger, ai.Namespace, snap.secretNames, name, "refreshed"); err != nil {
			heldBack[name] = prevSnap
			errs = append(errs, err)
		}
	}
	// ADDED (name in current, absent in prev) is intentionally NOT emitted.

	next := advance(current, heldBack)
	p.observed[key] = next
	ai.Status.ObservedCredentials = observedCredentialsForStatus(next)
	return errors.Join(errs...)
}

// advance builds the credential set to record: the current spec, with every
// entry whose revoke failed to publish rolled back to the value it had
// before — including entries the spec no longer mentions, which must stay in
// the record or the removal would be forgotten.
func advance(current, heldBack map[string]credentialSnapshot) map[string]credentialSnapshot {
	next := make(map[string]credentialSnapshot, len(current)+len(heldBack))
	for name, snap := range current {
		next[name] = snap
	}
	for name, snap := range heldBack {
		next[name] = snap
	}
	return next
}

// emitAll publishes one revoke per backing Secret of a credential and returns
// the joined error if any of them failed.
func (p *RevokePublisher) emitAll(ctx context.Context, logger logr.Logger, secretNs string,
	secretNames []string, credName, reason string) error {
	var errs []error
	for _, secretName := range secretNames {
		if err := p.emit(ctx, logger, secretNs, secretName, credName, reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// emit publishes one credential revoke on the unified ap.revocation bus.
// The revoke key is "<secretNamespace>/<secretName>" — the same shape the
// credential Invalidator parses to call broker.InvalidateSecret. Errors are
// logged (per the "never silently drop errors" AGENTS.md rule) AND returned,
// so the caller can hold the trigger state back and requeue.
func (p *RevokePublisher) emit(ctx context.Context, logger logr.Logger, secretNs, secretName, credName, reason string) error {
	if p.Bus == nil {
		// Local-dev / unit-test path: still log so an operator running
		// without NATS can see the diff was detected. Not an error — there is
		// nothing to retry, so holding the trigger state back would re-log
		// this line on every reconcile forever.
		logger.Info("revoke publisher: bus nil; skipping publish",
			"cred", credName, "reason", reason,
			"secretNs", secretNs, "secretName", secretName)
		return nil
	}
	key := secretNs + "/" + secretName
	// Credential revokes are emitted CLUSTER-WIDE (scope ""): the runner-side
	// subscriber registers with the session's namespace, and a scope-gated emit
	// would be dropped by revocation.Applies for any session not in secretNs.
	// The key is "<secretNamespace>/<secretName>" — fully namespace-qualified
	// and globally unique — so broker.InvalidateSecret only ever drops the one
	// matching cache entry regardless of which runner receives it (a miss is a
	// safe no-op). The scope gate exists only for tool-origin revokes, whose
	// keys aren't namespace-qualified. (AgentIdentity happened to work because
	// ai.Namespace == session ns, but cluster-wide is correct + robust.)
	if err := p.Bus.Emit(ctx, credentialRevokeKind, key, ""); err != nil {
		logger.Info("revoke publisher: emit failed; holding the trigger state back so the next reconcile retries",
			"err", err.Error(),
			"cred", credName, "reason", reason, "key", key)
		return err
	}
	logger.Info("credential change published",
		"cred", credName, "reason", reason, "key", key)
	return nil
}

// snapshotsFromStatus rebuilds the in-memory snapshot map from the durable
// record. An entry with an empty name is dropped: it can only come from a
// hand-edited status and would otherwise shadow a real credential.
func snapshotsFromStatus(recs []spiceboxv1alpha1.ObservedCredential) map[string]credentialSnapshot {
	out := make(map[string]credentialSnapshot, len(recs))
	for _, r := range recs {
		if r.Name == "" {
			continue
		}
		out[r.Name] = credentialSnapshot{
			fingerprint: r.Fingerprint,
			secretNames: append([]string(nil), r.SecretNames...),
		}
	}
	return out
}

// observedCredentialsForStatus renders the observed set for
// status.observedCredentials, sorted by name so an unchanged credential set
// produces a byte-identical value and the status write stays a no-op. An
// empty set renders as nil, not [], so it round-trips through omitempty and
// compares equal to the zero value under apiequality.Semantic.
func observedCredentialsForStatus(snaps map[string]credentialSnapshot) []spiceboxv1alpha1.ObservedCredential {
	if len(snaps) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.ObservedCredential, 0, len(snaps))
	for name, snap := range snaps {
		rec := spiceboxv1alpha1.ObservedCredential{Name: name, Fingerprint: snap.fingerprint}
		if len(snap.secretNames) > 0 {
			rec.SecretNames = append([]string(nil), snap.secretNames...)
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// agentCredentialSnapshot builds the credentialSnapshot for one credential.
// The fingerprint encodes "<type>:<secretRefName>" — stable across reconciles,
// sensitive to type changes (static↔oauth) and Secret-ref renames (which is
// how OAuth refresh surfaces a new credential). secretNames captures the
// backing Secret name(s) so REMOVED credential revokes can be issued without
// API calls.
//
// Returns a snapshot with empty fingerprint and no secretNames when the
// discriminator block is missing — that's a malformed AgentCredential but
// we don't want a nil dereference here; the controller's spec-shape check
// would have already set Valid=False.
func agentCredentialSnapshot(logger logr.Logger, c spiceboxv1alpha1.AgentCredential) credentialSnapshot {
	k, err := credkindregistry.Get(c.Type)
	if err != nil {
		// Unregistered type: this credential cannot be located and so cannot be
		// revoked by Secret name if it is later removed or replaced — log per
		// the no-silent-errors rule rather than let a rotation quietly not
		// revoke. The fingerprint still lets ADDED/REMOVED detection work.
		logger.Info("revoke publisher: credential has an unregistered type; it will not be revocable by Secret name",
			"credential", c.Name, "type", c.Type, "err", err.Error())
		return credentialSnapshot{fingerprint: c.Type + ":"}
	}
	ref := k.SecretRef(c)
	if ref == nil || ref.Name == "" {
		// No backing Secret for this type (e.g. federated is minted, not
		// stored), or a malformed/nil sub-block — nothing to revoke by Secret
		// name. Not logged: this is the type's documented shape, not an error.
		return credentialSnapshot{fingerprint: c.Type + ":"}
	}
	return credentialSnapshot{
		fingerprint: c.Type + ":" + ref.Name,
		secretNames: []string{ref.Name},
	}
}
