// attested_edge.go persists the answer the platform already had and threw
// away. Every verification of a forge credential asks the provider who owns
// the token; VerifyResult.SubjectID carries that answer back, and the link
// step records it on the credential's Secret. This file turns that recorded
// observation into two durable graph edges:
//
//	github_user:<numeric id>#user@user:<canonical of spec.subject>
//	github_user:<numeric id>#sole_user@user:<canonical of spec.subject>
//
// #user says who someone IS. It confers no permission — `user` is a relation
// on a type that exposes none (see the github kind's schema fragment) — and
// is written for every binding, regardless of how many other subjects already
// claim the same account.
//
// #sole_user is DERIVED, level-triggered state: recomputed on every pass from
// the LIVE claimant count (liveAttestedClaimants) — not from the stored #user
// tuples #user's own conflict notice reads — written only while exactly one
// subject claims the account, and withdrawn the instant a second appears,
// from the whole object, so both (or all) claimants lose it. Reading live
// state rather than stored tuples is what makes the withdrawal RECOVERABLE:
// a stored tuple is never deleted, so counting it would make a withdrawal
// permanent. Durable authority (repository roles, once a directory sync
// wires that consumer) traverses #sole_user only, never #user.
//
// The recomputation above is reached through a CREDENTIAL, which leaves one
// change it can never observe: the last claim on an account going away. There
// is then no credential to recompute from, and mapCoClaimants has no other
// claimant to enqueue. withdrawUnclaimedSoleIdentities closes that by asking
// the graph which accounts this subject still holds #sole_user on and
// withdrawing the ones the catalog no longer claims; a finalizer on the
// UserIdentity is what gives a DELETED catalog a last pass to do it in.
//
// Whose identity it binds is read from UserIdentity.spec.subject, the record
// that already declares whose catalog this is, and never inferred from
// whoever happened to paste the token.

package useridentity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	platformuseridentity "github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// AttestedIdentityWriter is this controller's narrow view of SpiceDB, satisfied
// by *spicedb.Client. LookupAttestedIdentitySubjects answers "is somebody else
// already bound to this account, per the STORED tuples?" — the question the
// #user conflict NOTICE below still answers from history. TouchAttestedIdentity
// writes #user, unconditionally, for every binding. TouchSoleIdentity and
// DeleteSoleIdentity write and withdraw #sole_user — derived, level-triggered
// state, but recomputed from LIVE UserIdentity CRs (liveAttestedClaimants),
// deliberately NOT from LookupAttestedIdentitySubjects: a stored #user tuple is
// never deleted, so gating #sole_user on it would make a withdrawal PERMANENT
// the first time a second claimant ever appeared, with no way back even after
// the account is unlinked. LookupSoleIdentitySubjects answers a third,
// narrower question — "did THIS pass just take sole_user away?" — which is
// what lets a live-only withdrawal (one the stored view can never see) be
// reported exactly once instead of never, or on every single pass. See
// reconcileAttestedEdge for all three derivations.
//
// LookupSoleIdentityAccounts reads sole_user from the OTHER end — which
// accounts does this subject still hold it on — and is the only question the
// catalog itself cannot answer. Every derivation above recomputes from the
// credentials a pass READ, so an account this catalog has STOPPED claiming is
// never visited again and its tuple would outlive the claim forever; see
// withdrawUnclaimedSoleIdentities.
//
// Declared as an interface here rather than taking *spicedb.Client so this
// package does not depend on the SpiceDB client, and so a Reconciler built
// without one holds a genuine nil (see the field's own comment).
type AttestedIdentityWriter interface {
	LookupAttestedIdentitySubjects(ctx context.Context, objType, subjectID string) ([]string, error)
	TouchAttestedIdentity(ctx context.Context, objType, subjectID string, canonicalID identity.CanonicalUserID) error
	TouchSoleIdentity(ctx context.Context, objType, subjectID string, canonicalID identity.CanonicalUserID) error
	DeleteSoleIdentity(ctx context.Context, objType, subjectID string) error
	LookupSoleIdentitySubjects(ctx context.Context, objType, subjectID string) ([]string, error)
	LookupSoleIdentityAccounts(ctx context.Context, objType string, canonicalID identity.CanonicalUserID) ([]string, error)
}

// attestedObjectTypes maps a credential provider id to the SpiceDB object type
// that keys that provider's accounts. A provider with no entry writes NOTHING:
// guessing a type would mint an edge into a definition the schema may not
// even hold, and an identity claim nobody can interpret is worse than none.
//
// Both sides are literals because neither has an importable constant today.
//
// The KEY is the provider's catalog id — the `id:` field of a file under
// /providers/, which is what the attestation records (see
// pkg/platform/identity/authkind/contract.go's ProviderID, and its writer in
// pkg/platform/identity/useridentity/store.go). It is NOT the setup flow's
// registry name: providers/github-pat.yaml happens to spell `id:` and
// `builtin:` the same, so the two coincide here and only here. A provider
// whose two differ must be keyed by its `id:`, or it writes no edge at all —
// silently, since an unmapped provider is a deliberate no-op.
//
// The VALUE is a definition in the base scaffold (pkg/authz/spicedb/schema/
// schema.zed's github_user). Reaching for either side would drag the
// provider catalog or the scaffold into the operator's controller; a new row
// is cheap and this comment names what to check.
var attestedObjectTypes = map[string]string{
	"github-pat": "github_user",
}

// agentIdentityOwnerKind is the adoption owner kind an AgentIdentity stamps.
// Matches the literal the AgentIdentity reconciler passes to adoptkit.
const agentIdentityOwnerKind = "AgentIdentity"

// now returns the reconcile-time clock, defaulting to time.Now when unset.
// clock is the test seam; production has exactly one clock, so nothing wires it.
func (r *Reconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// reconcileAttestedEdges mints the identity edge for every credential whose
// Secret carries a link-time attestation. secs are the Secrets this reconcile
// already read and validated, in credential order, so nothing is re-fetched
// and the write order is deterministic.
//
// Errors are joined rather than short-circuited: one provider being
// unreachable must not decide whether the next credential's edge is written.
//
// The withdrawal sweep runs afterwards, and only on a pass where every
// credential's edge landed. Its whole input is "which accounts does this
// catalog still claim", built from the very calls above; a pass that failed
// part way has an incomplete answer, and an incomplete answer here reads as
// "no longer claimed" and revokes. Skipping the sweep costs a requeue.
func (r *Reconciler) reconcileAttestedEdges(ctx context.Context, u *spiceboxv1alpha1.UserIdentity, secs []*corev1.Secret) error {
	var errs []error
	claimed := map[string]bool{}
	for _, sec := range secs {
		account, err := r.reconcileAttestedEdge(ctx, u, sec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if account != "" {
			claimed[account] = true
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return r.withdrawUnclaimedSoleIdentities(ctx, u, claimed)
}

// withdrawUnclaimedSoleIdentities withdraws #sole_user from every account this
// catalog's subject still holds it on and no longer claims.
//
// It exists because every other derivation in this file is driven BY a
// credential: reconcileAttestedEdge recomputes an account's sole_user from the
// Secret that named it, so an account whose credential is gone is never
// visited again, and mapCoClaimants enqueues only the account's OTHER
// claimants — of which, for the case that matters, there are none. Removing
// the last credential (or deleting the whole UserIdentity) therefore left
// sole_user standing, and durable authority derived from it — repository
// roles the directory sync keeps rewriting — standing with it, with no shipped
// path to remove the tuple: TypedWritesSource claims the relation, so no
// bootstrap or CLI could touch it either.
//
// Level-triggered like the rest, and driven from the graph rather than from
// remembered state: it asks SpiceDB which accounts this subject still holds
// sole_user on, and withdraws each one the catalog no longer claims. That
// makes it self-healing — an install that already leaked a tuple is cleaned up
// by the next pass of the catalog that leaked it — and needs nothing recorded
// on the CR, which would be a second copy of a truth the graph already holds.
//
// stillClaimed is this pass's own answer, keyed "<objType>:<subjectID>"; nil
// means "claims nothing", which is what the finalizer passes for a catalog on
// its way out.
//
// liveAttestedClaimants is consulted before each withdrawal so that two
// UserIdentity CRs naming the SAME subject (nothing forbids it) do not revoke
// each other's tuple: the one asking may have stopped claiming the account
// while its sibling has not.
func (r *Reconciler) withdrawUnclaimedSoleIdentities(ctx context.Context, u *spiceboxv1alpha1.UserIdentity,
	stillClaimed map[string]bool) error {
	if r.SpiceDB == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	canonical, err := identity.Subject(u.Spec.Subject).CanonicalUserID()
	if err != nil {
		// Not a user subject, so it can hold no user-typed edge and there is
		// nothing to look up. Logged at the same level reconcileAttestedEdge
		// logs the same skip, and for the same reason.
		logger.V(1).Info("useridentity: spec.subject is not a user subject; no sole-identity withdrawal to consider",
			"userIdentity", u.Name, "subject", u.Spec.Subject, "err", err.Error())
		return nil
	}
	var errs []error
	for _, objType := range attestedObjectTypeSet() {
		held, err := r.SpiceDB.LookupSoleIdentityAccounts(ctx, objType, canonical)
		if err != nil {
			// Retryable: a withdrawal that never ran must be attempted again,
			// not treated as "nothing to withdraw".
			errs = append(errs, fmt.Errorf("useridentity %s: look up %s accounts held by %s: %w",
				u.Name, objType, u.Spec.Subject, err))
			continue
		}
		for _, subjectID := range held {
			if stillClaimed[objType+":"+subjectID] {
				continue
			}
			claimants, err := r.liveAttestedClaimants(ctx, objType, subjectID)
			if err != nil {
				errs = append(errs, fmt.Errorf("useridentity %s: derive live claimants for %s:%s: %w",
					u.Name, objType, subjectID, err))
				continue
			}
			if claimants[canonical.String()] {
				continue // another live catalog for this same subject still claims it
			}
			if err := r.SpiceDB.DeleteSoleIdentity(ctx, objType, subjectID); err != nil {
				errs = append(errs, fmt.Errorf("useridentity %s: withdraw sole identity edge %s:%s: %w",
					u.Name, objType, subjectID, err))
				continue
			}
			logger.Info("useridentity: withdrew sole_user from an account this catalog no longer claims; durable authority derived from it is revoked",
				"userIdentity", u.Name, "objectType", objType, "attestedSubject", subjectID, "subject", u.Spec.Subject)
		}
	}
	return errors.Join(errs...)
}

// attestedObjectTypeSet is attestedObjectTypes' distinct VALUES, sorted — the
// object types a subject can hold an attested edge on, independent of how many
// providers map onto each. Sorted so a pass's withdrawal order is
// deterministic rather than map-order.
func attestedObjectTypeSet() []string {
	seen := map[string]bool{}
	var out []string
	for _, objType := range attestedObjectTypes {
		if seen[objType] {
			continue
		}
		seen[objType] = true
		out = append(out, objType)
	}
	sort.Strings(out)
	return out
}

// reconcileAttestedEdge writes at most one edge for one credential Secret.
//
// It returns an error only for a failure that must be RETRIED — a SpiceDB
// lookup or write that did not land. Every deliberate skip (no attestation, an
// unmapped provider, a shared credential, a non-user subject, no SpiceDB
// wiring) is a logged no-op, because none of them will resolve on a requeue.
//
// account is "<objType>:<subjectID>" when this credential makes the catalog a
// CLAIMANT of that provider account, and empty for every skip above — a
// shared bot credential included, which names no human and so claims nothing.
// The caller's withdrawal sweep reads it as this pass's whole answer to "what
// does this catalog claim", so a path that does not reach the sole-identity
// gate must not report one.
func (r *Reconciler) reconcileAttestedEdge(ctx context.Context, u *spiceboxv1alpha1.UserIdentity, sec *corev1.Secret) (account string, err error) {
	logger := log.FromContext(ctx)

	providerID, subjectID, ok := platformuseridentity.Attestation(sec)
	if !ok {
		// Most credentials are not forge credentials, and a half-written
		// attestation is not a claim about an unknown half — Attestation
		// requires both.
		return "", nil
	}
	objType, ok := attestedObjectTypes[providerID]
	if !ok {
		logger.V(1).Info("useridentity: attested credential names a provider with no attested-identity object type; writing nothing",
			"userIdentity", u.Name, "provider", providerID, "secret", sec.Name)
		return "", nil
	}

	// A credential an AgentIdentity ALSO holds is a shared bot token, and an
	// AgentIdentity names no human. Minting here would let whoever links a
	// shared bot token acquire that account's identity, which is the exact
	// claim the design forbids.
	shared, err := r.sharedWithAnAgentIdentity(ctx, sec)
	if err != nil {
		// Fail closed and retry. The edge is irreversible, so minting it on
		// the strength of an unanswered question is not a recoverable mistake.
		return "", fmt.Errorf("useridentity %s: %w", u.Name, err)
	}
	if shared {
		logger.V(1).Info("useridentity: credential is also held by an AgentIdentity; no attested identity edge (a shared credential names no single human)",
			"userIdentity", u.Name, "secret", sec.Name, "provider", providerID)
		return "", nil
	}

	// The catalog's DECLARED owner, never whoever pasted the token.
	canonical, err := identity.Subject(u.Spec.Subject).CanonicalUserID()
	if err != nil {
		// A UserIdentity whose subject is not a user subject has nothing a
		// user-typed identity edge can bind, and the schema would refuse the
		// tuple. Worth an operator's attention, hence Info.
		logger.Info("useridentity: spec.subject is not a user subject; no attested identity edge",
			"userIdentity", u.Name, "subject", u.Spec.Subject, "err", err.Error())
		return "", nil
	}

	if r.SpiceDB == nil {
		logger.V(1).Info("useridentity: no SpiceDB wiring; attested identity edge skipped",
			"userIdentity", u.Name, "objectType", objType, "attestedSubject", subjectID)
		return "", nil
	}
	account = objType + ":" + subjectID

	existing, err := r.SpiceDB.LookupAttestedIdentitySubjects(ctx, objType, subjectID)
	if err != nil {
		// Fail closed and retry: writing without the lookup would land the
		// edge while silently losing the one collision notice a human gets.
		return "", fmt.Errorf("useridentity %s: lookup attested identity subjects for %s:%s: %w", u.Name, objType, subjectID, err)
	}

	bound := false
	var others []string
	for _, s := range existing {
		if s == canonical.String() {
			bound = true
			continue
		}
		// s is a canonical id THIS platform wrote into the attested-identity
		// tuple (see TouchAttestedIdentity below) and is reading back only to
		// render the collision notice. CanonicalUserID became an unforgeable
		// struct, so a bare string cast no longer compiles; the id is trusted
		// because we stored it.
		others = append(others, identity.CanonicalFromTrusted(s, "read back from the attested-identity tuple SpiceDB stores").Subject().String())
	}
	// sole_user is derived from the LIVE claimant count — NOT from `others`
	// above, which reads stored SpiceDB tuples. A stored #user tuple is never
	// deleted (this reconciler has no unbind path; see
	// sharedWithAnAgentIdentity's own doc comment on the same residual
	// limit), so gating #sole_user on it would make a withdrawal PERMANENT
	// the instant a second claimant EVER appeared — with no way back even
	// after the second claimant unlinks, which breaks the very recoverability
	// the conflict notice below promises. Counting live UserIdentity CRs
	// instead means deleting a CR (or just the one credential naming this
	// account) removes it from the count on the very next pass this account
	// gets.
	//
	// Getting a pass is the part this count cannot supply on its own, and this
	// comment used to claim otherwise. Recomputation here is reached THROUGH a
	// credential, so the LAST claim going away is the one change that leaves
	// nothing to recompute from: the sweep in withdrawUnclaimedSoleIdentities
	// covers it, and the UserIdentity finalizer gives the deletion case a pass
	// to run in.
	//
	// Runs unconditionally, ABOVE the `bound` early return below: a second
	// claimant can appear on this account AFTER this catalog was already
	// bound, and that pass must still withdraw the edge — sitting this below
	// the early return would make that exact case (the one that matters) skip
	// the reconciliation entirely, because a catalog that has been bound since
	// before the conflict arose always finds bound == true.
	//
	// Fail closed on purpose: two subjects legitimately claiming one account is
	// a normal case, and nothing here can tell that from a borrowed
	// credential — so neither keeps durable authority while it is ambiguous.
	// A UserIdentity that has never itself reconciled still counts as a live
	// claimant: the attestation is stamped on its Secret at LINK time, before
	// any controller looks at it, so an unobserved second claimant must still
	// withdraw sole_user from the first.
	liveClaimants, err := r.liveAttestedClaimants(ctx, objType, subjectID)
	if err != nil {
		return "", fmt.Errorf("useridentity %s: derive live claimants for sole identity %s:%s: %w", u.Name, objType, subjectID, err)
	}
	// othersLive is the live-CR counterpart to `others` above: every OTHER
	// live claimant's subject, instead of every other STORED #user tuple.
	// The two can disagree in either direction — a stored tuple survives a
	// deleted UserIdentity (in `others`, not here), and a live, attested
	// credential can exist whose own reconcile never wrote anything (here,
	// not in `others`) — and it is exactly that second disagreement the
	// notice below exists to catch.
	var othersLive []string
	for cid := range liveClaimants {
		if cid == canonical.String() {
			continue
		}
		othersLive = append(othersLive, identity.CanonicalFromTrusted(cid,
			"resolved from a live UserIdentity CR's own attested credential").Subject().String())
	}
	if len(liveClaimants) == 1 && liveClaimants[canonical.String()] {
		if err := r.SpiceDB.TouchSoleIdentity(ctx, objType, subjectID, canonical); err != nil {
			return "", fmt.Errorf("useridentity %s: write sole identity edge %s:%s: %w", u.Name, objType, subjectID, err)
		}
	} else {
		// Look up who currently holds sole_user BEFORE withdrawing it. This is
		// the only way to tell "this pass is the one taking it away" from "it
		// was already gone" — `bound` cannot serve that role here, because
		// this branch runs precisely on catalogs `bound` already excludes from
		// the notice below (a second claimant can appear long after this one
		// was bound). Once withdrawn, priorSole is empty on every later pass
		// for as long as the collision persists, so the notify below fires at
		// most once per loss.
		//
		// A lookup FAILURE must not block the withdrawal below: this branch
		// exists because a contested account must not keep durable authority,
		// and that is a WRITE-side guarantee, not a read-side one. The sibling
		// early return at the top of this function (the "existing" lookup
		// that decides `bound`/`others`) blocks a WRITE on failure, which is
		// fail-closed for THAT write; returning here before DeleteSoleIdentity
		// would instead retain an already-granted sole_user for as long as this
		// read keeps failing, which is fail-OPEN on exactly the durable
		// authority this whole mechanism exists to gate. So priorSole is
		// treated as UNKNOWN (never as "empty", which would wrongly suppress
		// the notify below, and never as "non-empty", which would wrongly
		// fire it) on a lookup failure, the delete still runs, and the error is
		// surfaced afterward so the pass requeues and the notice can still land
		// once the read path recovers.
		priorSole, lookupErr := r.SpiceDB.LookupSoleIdentitySubjects(ctx, objType, subjectID)
		if lookupErr != nil {
			logger.Info("useridentity: lookup sole identity subjects failed; withdrawing sole_user anyway (fail-closed) rather than retain it for a contested account while the read path is degraded",
				"userIdentity", u.Name, "objectType", objType, "attestedSubject", subjectID, "err", lookupErr.Error())
		}
		deleteErr := r.SpiceDB.DeleteSoleIdentity(ctx, objType, subjectID)
		// The stored-tuple notice below (keyed on `others`, gated by `bound`)
		// already reports this SAME collision once the other claimant's own
		// #user pass runs. This is the complementary, additional trigger for
		// when it never does: the other claimant is live (othersLive) but
		// invisible to the stored view (others empty) — its own catalog is
		// Invalid for an unrelated reason, say, and never reaches
		// reconcileAttestedEdges at all. Silence is the one outcome this
		// design forbids: see publishAttestedConflict's own doc comment.
		//
		// Gated on lookupErr == nil: an unknown priorSole must never be read as
		// "held" (would over-notify were the account never contested this way)
		// or as "not held" (would silently skip a real, reportable loss).
		//
		// len(others) == 0 is load-bearing on its own, not merely belt-and-
		// braces: when the FIRST claimant was already sole-and-stored (its own
		// #user tuple written, priorSole naming it), a SECOND claimant's own
		// not-yet-bound pass has BOTH othersLive>0 and priorSole>0 true
		// regardless of this clause — the standard, `others`-keyed trigger
		// further down is what reports that arrival, and without this clause
		// this trigger would ALSO fire in the same pass, emitting two
		// identical notices for one conflict. See
		// TestAttestedEdge_SoleAndStoredFirstClaimantDoesNotDoubleNotifyWhenASecondArrives.
		if lookupErr == nil && deleteErr == nil && len(others) == 0 && len(othersLive) > 0 && len(priorSole) > 0 {
			r.publishAttestedConflict(ctx, u, objType, subjectID, othersLive)
		}
		var errs []error
		if lookupErr != nil {
			errs = append(errs, fmt.Errorf("lookup sole identity subjects for %s:%s: %w", objType, subjectID, lookupErr))
		}
		if deleteErr != nil {
			errs = append(errs, fmt.Errorf("withdraw sole identity edge %s:%s: %w", objType, subjectID, deleteErr))
		}
		if err := errors.Join(errs...); err != nil {
			return "", fmt.Errorf("useridentity %s: %w", u.Name, err)
		}
	}

	if bound {
		// Already ours. Re-writing would be harmless (TOUCH), but re-reporting
		// would hand a human the same conflict notice on every resync. The
		// sole_user reconciliation above already ran regardless of this return.
		//
		// Still a CLAIM on this account, and the returned account says so: it is
		// exactly the steady state (bound long ago, nothing to write today) the
		// withdrawal sweep must not mistake for "no longer claimed".
		return account, nil
	}
	// SAFETY DEPENDENCY — read before changing this write to a veto.
	//
	// When `others` is non-empty a DIFFERENT subject already claims this forge
	// account, and this still writes a SECOND #user binding (notice, not veto,
	// unchanged). That is safe because #user itself confers no permission
	// beyond the one reviewed, session-scoped consumer described below, and
	// because durable authority no longer traverses #user at all: a directory
	// sync's repo-access join is wired to github_user#sole_user instead (see
	// TouchSoleIdentity/DeleteSoleIdentity above), which this account holds
	// only while exactly one subject claims it. A second claimant therefore
	// gets a recorded #user binding and a conflict notice, but repository
	// access derived from this account is withdrawn from every claimant, not
	// handed to the new one.
	//
	// The one reviewed #user consumer is agentsession owner/participant/denied,
	// via the github kind's SessionRelationLinks: a pull-request-triggered
	// session names its PR author as an owner by GitHub account. A second
	// binding confers standing on that account's OWN pull-request sessions —
	// judged compatible with notice-not-veto because both claimants hold the
	// account's own verified credential (this reconciler mints from nothing
	// else), what is conferred is attributable and session-scoped rather than
	// repository access, and the composer's parity puts the link type in
	// `denied` so it can be rescinded per session. That allowlist is pinned by
	// guardian/schema.TestGithubUserBinding_OnlyAgentsessionConsumesIt, which
	// is now relation-aware: it scopes its check to #user specifically, so a
	// #sole_user consumer (the repo-access join) does not trip it, and any
	// OTHER #user consumer still fails the test by design.
	//
	// A blanket veto on #user is deliberately NOT used: one human
	// authenticating through two channels has two subjects, so two subjects
	// legitimately claiming one account is a normal case, not an attack — and
	// the genuinely dangerous case, a SHARED bot credential, is already
	// refused above via sharedWithAnAgentIdentity.
	if err := r.SpiceDB.TouchAttestedIdentity(ctx, objType, subjectID, canonical); err != nil {
		return "", fmt.Errorf("useridentity %s: write attested identity edge %s:%s: %w", u.Name, objType, subjectID, err)
	}
	// Reported only once the edge exists, and therefore exactly once: the next
	// pass finds this catalog among the bound subjects and returns above.
	//
	// The order is what bounds it. Notifying first would re-notify on every
	// requeue for as long as the WRITE kept failing — the conflict stays
	// unresolved, so the same notice is regenerated each pass — which is a
	// stream of duplicate warnings onto a cluster that is already degraded.
	// Notifying after also means the notice never describes an edge that does
	// not exist.
	if len(others) > 0 {
		r.publishAttestedConflict(ctx, u, objType, subjectID, others)
	}
	return account, nil
}

// sharedWithAnAgentIdentity reports whether an AgentIdentity also holds sec.
//
// It asks TWO questions, because one is a fact and the other is a race.
//
// The adoption annotation is authoritative once stamped — annotations
// accumulate, one key per owner, so being reached through a UserIdentity says
// nothing about whether an agent holds it too. But it is stamped by the
// AgentIdentity RECONCILER, and this one may run first. A Secret an
// AgentIdentity already REFERENCES is just as shared as one it has adopted, so
// a bot credential declared and linked in the same breath would otherwise mint
// a permanent human identity in the window before adoption lands.
//
// Only AgentIdentities in IdentitiesNamespace are considered, and that is
// exhaustive rather than a shortcut: a credential's secretRef resolves in its
// own object's namespace, so no AgentIdentity elsewhere can name this Secret.
// The read goes through the cached client, which this controller already
// depends on for the same namespace's Secrets.
//
// The residual limit, stated rather than implied: this answers "is it shared
// NOW". Nothing deletes an edge already written (spec §6 makes that survival
// deliberate), so an AgentIdentity that adopts this Secret LATER inherits an
// edge minted before it existed. Closing that needs an unbind path, not a
// better read here.
func (r *Reconciler) sharedWithAnAgentIdentity(ctx context.Context, sec *corev1.Secret) (bool, error) {
	if adoptkit.HasOwnerOfKind(sec.Annotations, agentIdentityOwnerKind) {
		return true, nil
	}
	var list spiceboxv1alpha1.AgentIdentityList
	if err := r.Client.List(ctx, &list, client.InNamespace(spiceboxv1alpha1.IdentitiesNamespace)); err != nil {
		return false, fmt.Errorf("list AgentIdentities in %s: %w", spiceboxv1alpha1.IdentitiesNamespace, err)
	}
	for i := range list.Items {
		for _, c := range list.Items[i].Spec.Credentials {
			// Dispatch through the registry rather than switching on
			// c.Static/OAuth/Federated, same as the Secret watch mapper.
			name, err := credkindregistry.SecretNameFor(c)
			if err != nil {
				// An unregistered type resolves to no Secret name, so it can
				// neither match nor be shown not to. Treated as no match and
				// logged: calling it a match would suppress every edge in the
				// cluster over one malformed AgentIdentity, and the adoption
				// annotation still catches it once that identity reconciles.
				log.FromContext(ctx).Info("useridentity: AgentIdentity credential has an unregistered type; it cannot be checked for sharing this Secret",
					"agentIdentity", list.Items[i].Name, "credential", c.Name, "type", c.Type, "err", err.Error())
				continue
			}
			if name != "" && name == sec.Name {
				return true, nil
			}
		}
	}
	return false, nil
}

// liveAttestedAccountsFor returns the set of attested-account keys
// ("<objType>:<subjectID>") that ui's OWN credentials currently name — every
// credential whose backing Secret carries a link-time attestation mapping to
// a registered object type, and that is NOT shared with an AgentIdentity. nil
// (never an error) when ui's own spec.subject is not a user subject: nothing
// it holds can bind a user-typed identity edge, so it claims nothing.
//
// Factored out of liveAttestedClaimants so the SAME per-catalog resolution —
// adopt-then-read each credential Secret, check its attestation, exclude a
// bot-shared one — backs both "who else claims THIS account" (below) and
// "what does THIS catalog claim, so who else must be nudged"
// (mapCoClaimants's watch mapper).
//
// Each candidate Secret is adopted under ITS OWN catalog's name (the same
// call that catalog's own reconcile would make) before being read through the
// guarded SecretReader. Skipping that and reading straight through
// r.APIReader would dodge the very check adoptguard exists to enforce; the
// Secret genuinely IS this controller's business (a UserIdentity credential
// in IdentitiesNamespace), it just may not have been labeled yet, and
// adopting it here is exactly as legitimate as the reconcile of its own
// catalog doing so a moment later. That live read (a per-credential
// AdoptSecret SSA Patch, then a second live Get through the guard) is a write
// round-trip per credential per pass, not a cheap in-memory check — see the
// cost note on liveAttestedClaimants below.
func (r *Reconciler) liveAttestedAccountsFor(ctx context.Context, ui *spiceboxv1alpha1.UserIdentity) (map[string]bool, error) {
	if _, err := identity.Subject(ui.Spec.Subject).CanonicalUserID(); err != nil {
		return nil, nil // not a user subject; cannot claim a user-typed identity edge
	}
	logger := log.FromContext(ctx)
	ownerRef := types.NamespacedName{Name: ui.Name} // cluster-scoped; no namespace
	accounts := map[string]bool{}
	for _, c := range ui.Spec.Credentials {
		secretName, err := credkindregistry.SecretNameFor(c)
		if err != nil {
			logger.V(1).Info("useridentity: credential has an unregistered type; cannot check it for a live attested-account claim",
				"userIdentity", ui.Name, "credential", c.Name, "type", c.Type, "err", err.Error())
			continue
		}
		if secretName == "" {
			continue // this type has no backing Secret (e.g. federated); nothing to attest with
		}
		secretRef := types.NamespacedName{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secretName}
		if err := adoptkit.AdoptSecret(ctx, r.SecretReader.Reader, r.Client, secretRef, ownerRef, "UserIdentity"); err != nil {
			if apierrors.IsNotFound(err) {
				continue // referenced Secret does not exist; nothing to claim with
			}
			return nil, fmt.Errorf("adopt Secret %s for UserIdentity %s: %w", secretRef, ui.Name, err)
		}
		sec, err := r.SecretReader.Get(ctx, secretRef)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get Secret %s for UserIdentity %s: %w", secretRef, ui.Name, err)
		}
		providerID, subjectID, ok := platformuseridentity.Attestation(sec)
		if !ok {
			continue
		}
		objType, ok := attestedObjectTypes[providerID]
		if !ok {
			continue
		}
		shared, err := r.sharedWithAnAgentIdentity(ctx, sec)
		if err != nil {
			return nil, fmt.Errorf("check shared-with-AgentIdentity for Secret %s: %w", secretRef, err)
		}
		if shared {
			continue // a bot-shared credential names no single human; not a live claim
		}
		accounts[objType+":"+subjectID] = true
	}
	return accounts, nil
}

// liveAttestedClaimants returns the canonical subjects of every LIVE
// UserIdentity CR that currently claims <objType>:<subjectID> (see
// liveAttestedAccountsFor for what "claims" means). Keyed by canonical id so
// the caller can both count distinct claimants and check whether IT is
// (still) one of them.
//
// This is deliberately NOT `others`/`existing` above, which read stored #user
// tuples this reconciler never deletes. Reading live cluster state instead is
// what makes #sole_user's withdrawal RECOVERABLE: deleting a UserIdentity, or
// just the one credential naming an account, drops it out of this set on the
// very next reconcile, with no event to replay.
//
// Level-triggered recovery is not the same as a trigger, though, and the two
// were conflated here once: dropping out of this set only matters on a pass
// that RUNS, and the last claimant's removal is precisely the change after
// which nothing reconciles that account again. withdrawUnclaimedSoleIdentities
// is the other half, and the UserIdentity finalizer is what gives it a pass to
// run in.
//
// A CR being DELETED is not a live claimant. Counting one would make the
// finalizer's own withdrawal pass find itself and conclude the account is
// still claimed — the object is still listed, with a deletion timestamp, until
// its finalizers are released.
//
// A UserIdentity that has never itself been reconciled still counts: the
// attestation is stamped onto its Secret at LINK time
// (platformuseridentity.PutToken), before any controller looks at it, so its
// claim is real and visible here regardless of whether its own reconcile has
// run yet — fail-closed, per the same reasoning `bound`'s caller applies.
//
// Cost, recorded accurately rather than left to guesswork: this is a List
// (cached) plus, for every credential of every UserIdentity in the cluster, a
// live-read existence check + an SSA Patch (AdoptSecret) + a second live Get
// through the guard — a write round-trip per credential per pass, not a
// cheap in-memory scan. At a fleet of hundreds of linked accounts this is a
// real cost on every reconcile of any one attested catalog; no index exists
// today that would let a targeted List replace the fan-out. The SSA payload is a pure function of
// its inputs, so a re-apply is a genuine no-op — this is repeated WORK, not a
// correctness or idempotency problem.
func (r *Reconciler) liveAttestedClaimants(ctx context.Context, objType, subjectID string) (map[string]bool, error) {
	var list spiceboxv1alpha1.UserIdentityList
	if err := r.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list UserIdentities: %w", err)
	}
	target := objType + ":" + subjectID
	claimants := map[string]bool{}
	for i := range list.Items {
		ui := &list.Items[i]
		if ui.DeletionTimestamp != nil {
			continue // being deleted; its claim is on the way out, not live
		}
		accounts, err := r.liveAttestedAccountsFor(ctx, ui)
		if err != nil {
			return nil, fmt.Errorf("resolve live attested accounts for UserIdentity %s: %w", ui.Name, err)
		}
		if !accounts[target] {
			continue
		}
		canonical, err := identity.Subject(ui.Spec.Subject).CanonicalUserID()
		if err != nil {
			// Unreachable in practice: liveAttestedAccountsFor already returns nil
			// for a non-user subject, so accounts[target] above is false. Guarded
			// anyway rather than assumed, since canonicalizing a non-user subject
			// would panic on .String() below otherwise.
			continue
		}
		claimants[canonical.String()] = true
	}
	return claimants, nil
}

// mapCoClaimants is the watch mapper that makes sole_user's derivation
// actually EVENT-driven instead of waiting on the manager's own resync
// (unconfigured in this repo, so it defaults to ~10h). On every UserIdentity
// create/update/delete, it finds every OTHER live UserIdentity that shares
// ANY attested account with the object the event carries, and enqueues a
// reconcile for each — so both the WITHDRAWING pass (a second claimant
// appears) and the RESTORING pass (a claimant goes away) are driven by a
// real event, not merely by whatever eventually reconciles next.
//
// controller-runtime's own EnqueueRequestsFromMapFunc runs this function on
// BOTH the old and the new object for an Update event (see its own doc
// comment) — which is what makes the RESTORING case work without this
// function needing to diff old vs. new itself: `oap user-identity
// delete-token` removes a credential from spec.credentials in an UPDATE, and
// the OLD object (still carrying that credential, and — best-effort, since
// the Secret delete that follows is a separate, slightly later call — likely
// still able to read its Secret) is what surfaces the co-claimant this
// credential's removal should nudge. A missed enqueue on that race is not a
// correctness bug: it only delays recovery back to the resync, the same
// outcome as before this mapper existed.
//
// o that is not a *UserIdentity, or whose own attested accounts cannot be
// resolved, enqueues nothing and logs — best-effort, self-heals at the next
// resync, same posture as mapSecretToUsers in SetupWithManager.
func (r *Reconciler) mapCoClaimants(ctx context.Context, o client.Object) []reconcile.Request {
	ui, ok := o.(*spiceboxv1alpha1.UserIdentity)
	if !ok {
		return nil
	}
	accounts, err := r.liveAttestedAccountsFor(ctx, ui)
	if err != nil {
		log.FromContext(ctx).Info("useridentity: co-claimant watch mapper could not resolve this catalog's attested accounts; a co-claimant may not be enqueued promptly (self-heals at the next resync)",
			"userIdentity", ui.Name, "err", err.Error())
		return nil
	}
	if len(accounts) == 0 {
		return nil
	}
	var list spiceboxv1alpha1.UserIdentityList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("useridentity: co-claimant watch mapper failed to list UserIdentities; self-heals at the next resync",
			"userIdentity", ui.Name, "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == ui.Name {
			continue // the primary watch (For) already enqueues the changed object itself
		}
		otherAccounts, err := r.liveAttestedAccountsFor(ctx, other)
		if err != nil {
			log.FromContext(ctx).Info("useridentity: co-claimant watch mapper could not resolve a candidate co-claimant's attested accounts; it may not be enqueued promptly (self-heals at the next resync)",
				"userIdentity", other.Name, "err", err.Error())
			continue
		}
		for a := range accounts {
			if otherAccounts[a] {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: other.Name}})
				break
			}
		}
	}
	return out
}

// publishAttestedConflict reports a second claim on one provider account. It
// is a NOTICE about the #user edge, not a veto: that edge is written either
// way. The notice must say what the collision actually costs, though — a
// silent revocation is the worst version of this — so it names the
// #sole_user withdrawal too: durable authority derived from this account
// (repository roles, once a directory sync wires that consumer) is pulled
// from every claimant for as long as more than one remains.
//
// Called from two sites in reconcileAttestedEdge, each covering a case the
// other cannot: the `others`-keyed call (below, gated by `bound`) fires from
// a NEW claimant's own first pass, once its #user tuple is stored — the
// standard case. The `othersLive`-keyed call (above, gated by a
// held-then-withdrawn sole_user check) fires when the OTHER claimant's own
// pass never runs at all, so the first call would never happen.
//
// Refusing the #user edge would block the case where both people are present
// and have both chosen, and would do nothing about the case where one is
// not — a borrowed token leaves no collision to detect at all, because the
// real owner never links. So the platform records both claims, withdraws the
// durable half, and says so.
//
// A publish failure is logged and swallowed: the edge is the durable half, and
// an unreachable monitoring bus must never cost an identity binding.
func (r *Reconciler) publishAttestedConflict(ctx context.Context, u *spiceboxv1alpha1.UserIdentity,
	objType, subjectID string, existing []string) {
	if r.MonitoringPublish == nil {
		log.FromContext(ctx).V(1).Info("useridentity: attested identity conflict not reported (no monitoring publisher); the edge is still written",
			"userIdentity", u.Name, "objectType", objType, "attestedSubject", subjectID)
		return
	}
	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelWarning,
		Category:   "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind: "UserIdentity", Name: u.Name,
		},
		Condition: "AttestedIdentity",
		Reason:    "SharedProviderAccount",
		Summary: fmt.Sprintf(
			"%s:%s is now claimed by %s and %s; repository access derived from this account has been withdrawn from every claimant until the conflict is resolved",
			objType, subjectID, strings.Join(existing, ", "), u.Spec.Subject),
		Hint:      "confirm this is a deliberately shared account rather than a borrowed credential, then unlink it from every UserIdentity but one to restore access",
		Timestamp: r.now(),
	}
	if err := channelevents.PublishMonitoring(r.MonitoringPublish, ev); err != nil {
		log.FromContext(ctx).Info("useridentity: publish attested-identity conflict event failed",
			"userIdentity", u.Name, "objectType", objType, "attestedSubject", subjectID, "err", err.Error())
	}
}
