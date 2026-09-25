// Package credentialupdaterequest hosts the CredentialUpdateRequest reconciler:
// the operator-side decision of whether an agent's claim that a credential has
// died justifies putting a credential-entry card in front of a human.
//
// The decision logic itself lives in pkg/platform/identity/credupdate
// (ResolveOrigin maps a failing tool's origin to one credential; Determine
// applies the verdict table). This package supplies only the I/O Determine
// cannot perform itself: resolving the session's runtime identity, attempting
// the OAuth refresh, running the live provider probe, and enforcing the
// per-(session, credential) ask budget.
//
// It DETERMINES ONLY — it never mints a link, builds a card, or publishes.
// channelsd's CredentialUpdateWatcher does that, watching for this CR's phase to
// reach Open. So this reconciler writes ONLY its own CR status: never
// AgentSession.status.phase, which belongs to the agentsession controller and
// its own watch on this CR, and never a signed link, which needs the passthrough
// signing key that channelsd holds by a NON-OPTIONAL volume mount — the kubelet
// blocks its pod start when that Secret is missing, rather than letting the
// process silently degrade forever.
package credentialupdaterequest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
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
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
)

// Reconciler implements the CredentialUpdateRequest controller.
//
// Broker is declared as an INTERFACE-typed field and assigned only from a real
// non-nil value. Assigning a typed-nil concrete pointer would yield a non-nil
// interface that panics on its first method call.
type Reconciler struct {
	Client client.Client
	Broker broker.Broker

	// Now is the time source for the Open-request idle-TTL check. Nil
	// defaults to time.Now().UTC(); tests inject a fixed value.
	Now func() time.Time
	// IdleTTL bounds how long a request may sit Open before Reconcile marks it
	// Expired — the "nobody clicks" path. Deliberately this reconciler's OWN
	// knob, decoupled from AgentClass.Spec.Channels.IdleTTL: that governs how
	// long a channel-attached RUNNER blocks in await_user_message, a different
	// concern from how long a HUMAN has to see a DM, complete OIDC, and paste a
	// token. Reusing it gave every session a 5-minute window, or disabled expiry
	// entirely — parked forever — whenever Spec.Channels was nil and its
	// kubebuilder default never materialized. Zero here means
	// DefaultCredentialUpdateIdleTTL, so an unwired caller still gets a sane
	// window rather than silence.
	IdleTTL time.Duration
}

// DefaultCredentialUpdateIdleTTL is the fallback when Reconciler.IdleTTL is
// zero. It is an ALIAS of credupdate.DefaultAskWindow, never a restated copy:
// this is the middle term of the cross-binary ordering
// "tool wait <= park TTL <= link lifetime" that the assertions below enforce.
const DefaultCredentialUpdateIdleTTL = credupdate.DefaultAskWindow

// Compile-time ordering assertions, this site's half of the contract in
// credupdate/timing.go. Converting a negative constant to uint64 is a compile
// error, so each line statically asserts a non-negative difference: setting
// DefaultCredentialUpdateIdleTTL longer than channelsd's link lifetime (humans
// clicking near the deadline get a dead link) or shorter than the runner tool's
// wait floor (the tool blocks on an already-expired request) fails the BUILD
// rather than shipping a skew that only surfaces when a real park times out.
const (
	_ = uint64(credupdate.DefaultLinkLifetime - DefaultCredentialUpdateIdleTTL) // park TTL <= link lifetime
	_ = uint64(DefaultCredentialUpdateIdleTTL - credupdate.DefaultToolWait)     // tool wait <= park TTL
)

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Reconciler) idleTTL() time.Duration {
	if r.IdleTTL > 0 {
		return r.IdleTTL
	}
	return DefaultCredentialUpdateIdleTTL
}

// RBAC is narrowed to what this reconciler does: Get/List/Watch the main object
// (List drives the ask-budget check) and write status. It never
// Creates/Updates/Patches/Deletes the spec — the runner's meta tool creates the
// CR and owner-ref GC deletes it with the session — so those verbs are
// deliberately absent HERE. The RBAC sufficiency test fails the build if the
// generated ClusterRole grants more than the operator uses.
//
// The operator's ClusterRole does carry `create` on this resource, from a
// DELEGATION marker in the agentsession controller: the operator writes the
// per-session runner Role granting the meta tool `create`, and
// privilege-escalation prevention forbids granting a verb the granter lacks.
// That verb is the agentsession controller's need, so its marker lives there.
//
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=credentialupdaterequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=credentialupdaterequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=credentialupdaterequests/finalizers,verbs=update

// budgetLimit is how many prior SPENT asks for the same (session, resolved
// credential) are tolerated before a further ask refuses outright. At the limit
// a new request would be the third ask for the identical credential in one
// session — past helping a human and into pestering them. What makes an ask
// spent is askIsSpent, not "terminal"; see its doc.
const budgetLimit = 2

// Reconcile drives a CredentialUpdateRequest to a single terminal decision:
// Refused (with a determination the agent can read verbatim) or Open (with
// a published card). See the package doc and credupdate.Determine for the
// verdict table this steps toward.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cr spiceboxv1alpha1.CredentialUpdateRequest
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &cr); !cont {
		return ctrl.Result{}, err
	}
	// The object exactly as read, kept for writeStatus' merge patch: every
	// status write below sends only what THIS reconcile changed. See writeStatus.
	prior := cr.DeepCopy()

	// Phase gate. A freshly-created request — "" or the explicit Pending stamp,
	// which IsCredentialUpdateRequestTerminal pins non-terminal precisely so it
	// still gets processed — falls through to the determination pipeline. Open
	// has already been decided and has a live card, but is NOT done: it must be
	// re-checked every reconcile for the human having fixed the Secret or the
	// idle TTL elapsing, so it routes to reconcileOpen rather than back through
	// the decision pipeline, which would re-refresh, re-probe, and publish a
	// SECOND card. Every other phase, including an unrecognized value treated
	// conservatively as decided, has reached a terminal decision and stops.
	//
	// COUPLED: the fall-through arm's phase set is the same set askIsSpent treats
	// as UNSPENT, and the two must move together. Adding an undetermined phase
	// here but not there charges the session an ask for a request that decided
	// nothing — fail-closed, but wrong, and silent.
	switch cr.Status.Phase {
	case "", spiceboxv1alpha1.CredentialUpdateRequestPhasePending:
		// fall through to the determination pipeline below.
	case spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen:
		return r.reconcileOpen(ctx, &cr, prior)
	case spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed:
		// A follower: no card of its own, waiting on the canonical's. It is NOT
		// decided, so it must not fall to the default arm, which would freeze it
		// in a phase nothing ever moves it out of — a follower that never
		// unblocks.
		canonical, cerr := r.canonicalFor(ctx, &cr)
		if cerr != nil {
			logger.Info("credentialupdaterequest: re-reading the request this one collapsed onto failed",
				"credupdate", req.NamespacedName.String(), "err", cerr.Error())
			return ctrl.Result{}, cerr
		}
		if canonical != nil {
			if canonical.Status.Phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen {
				// Still a live card in front of a human; keep waiting. The
				// RequeueAfter is a BACKSTOP behind the canonical→followers enqueue
				// in SetupWithManager, never the mechanism: it makes propagation
				// converge even when an enqueue never arrives (a failed List, an
				// informer restart mid-handover), so no follower's settlement hangs
				// on a single event being delivered.
				return ctrl.Result{RequeueAfter: collapsedRecheckInterval}, nil
			}
			if spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(canonical.Status.Phase) {
				return r.settleWithCanonical(ctx, &cr, prior, canonical)
			}
		}
		// The request this one waited on is gone, or sits in a phase that is
		// neither a live card nor a settled answer. Either way nobody is looking
		// at anything on this session's behalf, so drop the pointer and
		// re-determine this session's OWN ask by falling through — staying
		// collapsed onto a dead pointer would block this agent for its whole
		// wait window.
		logger.Info("credentialupdaterequest: the request this one collapsed onto neither holds an open card "+
			"nor reached a decision; re-determining this request's own ask",
			"credupdate", req.NamespacedName.String(),
			"collapsedInto", namespacedRefLabel(cr.Status.CollapsedInto),
			"canonicalPhase", canonicalPhaseLabel(canonical))
		cr.Status.CollapsedInto = nil
		// fall through to the determination pipeline below.
	default:
		logger.Info("credentialupdaterequest: phase already decided; skipping reprocessing",
			"credupdate", req.NamespacedName.String(), "phase", cr.Status.Phase)
		return ctrl.Result{}, nil
	}

	sid, idRefusal, err := r.resolveIdentity(ctx, &cr)
	if err != nil {
		logger.Info("credentialupdaterequest: identity resolution failed",
			"credupdate", req.NamespacedName.String(), "err", err.Error())
		return ctrl.Result{}, err
	}
	if idRefusal != nil {
		return r.writeStatus(ctx, &cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, *idRefusal)
	}

	result, err := credupdate.ResolveOrigin(ctx, r.Client, credupdate.ResolveInput{
		Namespace:         cr.Namespace,
		Origin:            cr.Spec.Origin,
		Identity:          sid.ident,
		IdentityKind:      sid.kind,
		IdentityName:      sid.name,
		IdentityNamespace: sid.namespace,
		// Remap is NOT optional decoration: the MCP authkind suggests the
		// credential name the TOOL declares, while a SessionUserIdentity's
		// credentials are keyed by the POST-remap name. Omitting it makes every
		// remapped MCPServer resolve a name the identity does not contain, or —
		// worse, when the raw name also happens to exist — open a card naming the
		// WRONG, healthy credential. Every other production caller passes it.
		Remap: sid.remap,
	})
	if err != nil {
		logger.Info("credentialupdaterequest: ResolveOrigin failed",
			"credupdate", req.NamespacedName.String(), "origin", cr.Spec.Origin, "err", err.Error())
		return ctrl.Result{}, err
	}
	if result.Refusal != nil {
		return r.writeStatus(ctx, &cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, *result.Refusal)
	}

	// From here on resolution succeeded: record what Origin resolved to on
	// every remaining status write, refusal or not -- only the two
	// resolution-failure refusals above (NoCredential/AmbiguousCredential,
	// handled by result.Refusal) leave it empty, per the field's doc.
	ref := result.Ref
	cr.Status.ResolvedCredential = &ref

	// Identity-kind gate: fail closed on an IdentityKind this reconciler does not
	// recognize, before a credential ever reaches Determine. AgentIdentity — an
	// agent's own shared credential — reaches determination like any other,
	// because who may approve replacing one IS modelled:
	// `agentidentity#update_credential` in the SpiceDB schema, satisfied by the
	// AgentIdentity reconciler's platform-link write and re-checked at click time
	// by identityd.
	//
	// This stays an ALLOWLIST rather than a blacklist: an empty, mistyped, or
	// future-unknown kind must refuse here, not silently proceed as though it
	// were updatable. IsKnownIdentityKind's doc lists the spellings recognized.
	if !IsKnownIdentityKind(ref.IdentityKind) {
		return r.writeStatus(ctx, &cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, credupdate.Outcome{
			Tier:          credupdate.TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNotUpdatable,
			Reason:        "This credential's identity kind is not recognized, so it cannot be evaluated for replacement.",
		})
	}

	// ONE namespace List feeds both checks below, which ask different questions
	// of the same rows: "how many of MY past asks are already spent" and "does
	// somebody else hold a live card for this credential". Both are pure
	// functions over this slice, which halves the read and keeps two
	// near-identical filter loops from being mistaken for each other.
	peers, err := r.peerRequests(ctx, &cr)
	if err != nil {
		logger.Info("credentialupdaterequest: listing this namespace's other requests failed",
			"credupdate", req.NamespacedName.String(), "err", err.Error())
		return ctrl.Result{}, err
	}

	if budgetExceeded(peers, cr.Spec.SessionRef, ref) {
		return r.writeStatus(ctx, &cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, credupdate.Outcome{
			Tier:          credupdate.TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationBudgetExhausted,
			// Deliberately says the session RAISED its asks, not that it "asked a
			// human" -- a spent ask may have ridden a card that channelsd never
			// managed to deliver, and a request nobody was ever shown must not be
			// reported as having bothered somebody. Same honesty rule the follower
			// reason renderer is built around (followerReason).
			Reason: "This session has already raised the maximum number of requests to replace this exact " +
				"credential. Repeating the same request again would not help; report what is still failing instead.",
		})
	}

	// Collapse: several sessions sharing one dead credential must raise ONE card
	// at ONE human, not one card each.
	//
	// It sits AFTER the ask budget on purpose. The budget is charged per
	// (session, credential), so a session that has burned its asks for this
	// credential is refused outright rather than allowed to attach to somebody
	// else's card; reversed, two sessions could take turns following each other
	// and neither would ever exhaust its own budget. The other half of that
	// property lives in askIsSpent: the Collapsed status about to be written
	// SPENDS an ask immediately, not on settlement — otherwise a session could
	// pile asks onto one live card faster than that card resolves and the budget
	// check would keep passing.
	//
	// It sits BEFORE the refresh attempt and the live probe for reasons beyond
	// saving work: the canonical already ran the FULL determination against the
	// identical credential, same Secret and same provider, and came out at Open,
	// so re-running it can only reproduce that answer at N times the egress —
	// and an oauth refresh fired from a follower would race the human who is at
	// that moment pasting a replacement into the canonical's card.
	if canonical := canonicalOpenRequestFor(peers, handleForSource(ref.IdentityKind, result.Descriptor.Source)); canonical != nil {
		return r.collapseOnto(ctx, &cr, prior, canonical)
	}

	// AgentOwned rides into Determine because it changes the VERDICT, not only
	// the routing: an agent's own oauth credential has no value a human can
	// paste, so it must be refused here rather than broadcast as a card whose
	// only outcome is a 409 at click time and an expiry claiming nobody acted.
	in := credupdate.Input{
		CredType:   result.Descriptor.Source.Type,
		AgentOwned: ref.AgentOwned(),
	}

	// Provider metadata is looked up regardless of credential type -- it's a
	// local catalog read, no egress -- and feeds two things: the title in the
	// reason text, and the corroboration gate immediately below. The LIVE probe
	// further down is skipped for federated credentials specifically.
	prov, _ := provider.ByID(ref.ProviderID)
	if prov != nil {
		in.ProviderTitle = prov.Title
	}

	// The platform's OWN evidence, and the only input to Determine that does not
	// come from this reconciler's I/O. PRESENCE is the whole signal: the runner
	// records the entry only after classifying a real upstream failure against
	// the provider's declared authFailure shape, and REMOVES it the moment a call
	// to the same origin succeeds. An entry still here therefore means the last
	// thing the platform saw at this origin was an auth-shaped failure — exactly
	// what corroboration is defined as.
	//
	// It decides nothing on its own. Determine consults it only where the live
	// probe came back non-definitive; a probe saying the credential is LIVE beats
	// it outright, because an observed rejection beside a working token is a
	// scope problem that re-entering the same token cannot fix.
	//
	// The declared-shape conjunct re-derives, on the READING side, the
	// precondition the writing side already applies: an origin whose provider
	// declares no authFailure block is never recorded at all. On every honest
	// path it is therefore a no-op — the entry simply is not there. It matters
	// for the entry that IS there anyway: a status field only the runner can
	// write, whose forgery the AgentSession webhook structurally cannot pin,
	// because the runner is its legitimate author.
	//
	// Whether corroboration is available at all for a provider is a per-entry
	// catalog decision, made for exactly this reason: with no verify probe to
	// appeal to, corroboration becomes the SOLE evidence that can put a
	// credential-entry form in front of a human, so a manufacturable shape is a
	// phishing primitive rather than a weak signal. Enforcing that only inside
	// the runner would leave it enforced by the component whose word this flow
	// exists not to take. The operator loads the same embedded catalog, so
	// re-checking costs one map lookup, and it fails CLOSED on catalog skew: an
	// operator that no longer sees the shape declared refuses the card rather
	// than vouching on a rule it cannot read.
	in.Corroborated = sid.authFailure != nil && prov != nil && prov.AuthFailure != nil
	if sid.authFailure != nil && !in.Corroborated {
		// Never silent: this suppression is the difference between a card and no
		// card, and the agent's refusal text reads as though nothing was there.
		// An operator asking why a request was refused while an observation sits
		// on the session needs this line to find the answer.
		logger.Info("credentialupdaterequest: ignoring an auth-failure observation for a provider that declares no auth-failure shape; "+
			"it cannot have been recorded through the classification path, so it does not corroborate",
			"credupdate", req.NamespacedName.String(), "origin", cr.Spec.Origin, "provider", ref.ProviderID,
			"providerKnown", prov != nil)
	}

	if k, kerr := credkindregistry.Get(in.CredType); kerr != nil {
		// credresolve's own registry.Get already refuses an unregistered type
		// well before ResolveOrigin can produce a Descriptor, so this is
		// unreachable in practice — but it must still fail closed (skip the
		// refresh attempt) rather than panic on a nil Kind.
		logger.Info("credentialupdaterequest: unknown credential type; skipping refresh attempt",
			"credupdate", req.NamespacedName.String(), "credType", in.CredType, "err", kerr.Error())
	} else if k.NeedsRefresh() {
		if rerr := r.attemptRefresh(ctx, &in, result.Descriptor.Source); rerr != nil {
			logger.Info("credentialupdaterequest: oauth refresh attempt errored",
				"credupdate", req.NamespacedName.String(), "err", rerr.Error())
			return ctrl.Result{}, rerr
		}
	}

	// Never ask a human for what the machine can do: RefreshOK short-circuits
	// Determine's self-heal step regardless of Probe, so probing then is a live
	// call whose result can never change the verdict. And never probe a
	// credential Determine refuses on its SHAPE — a federated one, or an agent's
	// OWN oauth one. ProbeWouldNotHelp is Determine's own predicate, so the two
	// cannot drift into probing what the verdict ignores, or skipping a probe
	// the verdict needs.
	if !credupdate.ProbeWouldNotHelp(in) && !in.RefreshOK {
		in.Probe, in.ProbeDetail = r.probeCredential(ctx, prov, result.Descriptor)
	}

	outcome := credupdate.Determine(in)

	// Logged AFTER Determine, carrying the determination, because the
	// observation's presence is not the verdict: a federated credential, a
	// successful RefreshOK self-heal, and a live probe all end Refused with the
	// entry still on status. Logging earlier would announce "corroborated" for
	// each of them. Emitted whenever the observation exists, even when it
	// changed nothing, so the record shows what the reconciler had and not only
	// what it acted on.
	if in.Corroborated {
		logger.Info("credentialupdaterequest: the platform's own auth-failure observation was available for this decision",
			"credupdate", req.NamespacedName.String(), "origin", cr.Spec.Origin,
			"observedCount", sid.authFailure.Count, "observedAt", sid.authFailure.ObservedAt,
			"determination", outcome.Determination)
	}

	if outcome.Tier == credupdate.TierNone {
		return r.writeStatus(ctx, &cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, outcome)
	}

	// Record the backing Secret's identity and current content hash: the baseline
	// the Secret watch compares against to detect "a human just fixed it", set on
	// cr so the single status write below persists it with the Open transition.
	// This reconciler stops here — it mints no link and publishes no card;
	// channelsd's CredentialUpdateWatcher does that once phase reaches Open.
	secretNN := types.NamespacedName{Namespace: result.Descriptor.Source.Namespace, Name: result.Descriptor.Source.Name}
	var backingSecret corev1.Secret
	if serr := r.Client.Get(ctx, secretNN, &backingSecret); serr != nil {
		logger.Info("credentialupdaterequest: read backing Secret failed",
			"credupdate", req.NamespacedName.String(), "secret", secretNN.String(), "err", serr.Error())
		return ctrl.Result{}, fmt.Errorf("credentialupdaterequest: read backing Secret %s: %w", secretNN, serr)
	}
	cr.Status.CredentialSecretRef = &spiceboxv1alpha1.NamespacedRef{Namespace: secretNN.Namespace, Name: secretNN.Name}
	// The KEY rides along with the ref, and the baseline digest is scoped to it.
	// Source.Key is the credential's third coordinate: without it "the backing
	// Secret" names a container several credentials share, not this credential.
	// See secretContentHash.
	cr.Status.CredentialSecretKey = result.Descriptor.Source.Key
	cr.Status.CredentialSecretObservedHash = secretContentHash(backingSecret.Data, result.Descriptor.Source.Key)

	// The instant the human's wait window starts, recorded SET-ONCE. This is the
	// only place that writes it; reconcileOpen only reads it, so the deadline it
	// computes cannot walk forward on every reconcile. The nil guard makes a
	// second arrival here — a follower whose canonical vanished, re-determining
	// its own ask — keep the original instant rather than restart the window.
	if cr.Status.OpenedAt == nil {
		now := metav1.NewTime(r.now())
		cr.Status.OpenedAt = &now
	}

	return r.writeStatus(ctx, &cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, outcome)
}

// reconcileOpen handles a request already in phase Open: decided, but not done.
// Every reconcile — periodic, or triggered by the Secret watch — checks, in this
// ORDER:
//  1. Has the backing Secret's content changed since the card opened? Then a
//     human acted: mark Fulfilled so the agent's blocked call can retry. FIRST,
//     because when a fix and the idle deadline land on the same reconcile,
//     Expired would mislabel a request a human DID resolve, just at the last
//     moment.
//  2. Has it been Open longer than IdleTTL? Then mark Expired. A LATER
//     out-of-band credential update must NOT auto-resume the session, and
//     Expired is a distinct terminal phase from Fulfilled precisely so nothing
//     downstream mistakes a since-abandoned request for a resolved one. The
//     window runs from status.openedAt, not creation — see openedAt.
//
// Neither check touches cr.Status.Determination: the ORIGINAL verdict that
// justified the card is preserved, and only Phase and Reason change.
func (r *Reconciler) reconcileOpen(ctx context.Context, cr, prior *spiceboxv1alpha1.CredentialUpdateRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Secret-change check runs FIRST: if a human fixed the credential on the
	// exact reconcile the idle deadline elapses, Fulfilled must win over Expired.
	fulfilled, err := r.secretChangedSinceOpen(ctx, cr)
	if err != nil {
		logger.Info("credentialupdaterequest: checking backing Secret for change failed",
			"credupdate", cr.Namespace+"/"+cr.Name, "err", err.Error())
		return ctrl.Result{}, err
	}
	if fulfilled {
		return r.writeStatus(ctx, cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, credupdate.Outcome{
			Tier:          credupdate.TierNone,
			Determination: cr.Status.Determination,
			Reason:        "The credential was updated. Retry your call.",
		})
	}

	idleTTL := r.idleTTL()
	deadline := openedAt(cr).Add(idleTTL)
	if !r.now().Before(deadline) {
		// Determination happens here; publication happens in channelsd, a
		// SEPARATE process with its own silent skip paths (no InputChannel, no
		// started-by subject, no ResolvedCredential, a missing signing key). So a
		// request can sit Open the whole idleTTL with nobody ever having seen a
		// card, and "nobody updated the credential" would be FALSE — nobody was
		// asked. An empty InteractionRef is the discriminator: channelsd stamps
		// it in the SAME patch as CardDelivered=True, so the two cannot disagree.
		reason := "Nobody updated the credential before the wait window elapsed."
		if cr.Status.InteractionRef == "" {
			reason = "The credential-update request was never delivered to a human (no card was ever shown) before the wait window elapsed."
		}
		return r.writeStatus(ctx, cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, credupdate.Outcome{
			Tier:          credupdate.TierNone,
			Determination: cr.Status.Determination,
			Reason:        reason,
		})
	}
	return ctrl.Result{RequeueAfter: time.Until(deadline)}, nil
}

// openedAt returns the instant this request's wait window started: the recorded
// status.openedAt, or CreationTimestamp when there is none.
//
// The two are NOT interchangeable. Creation and determination are separated by
// however long the pipeline took — an operator restart, or minutes of probe
// backoff against an unreachable provider — so a deadline measured from creation
// can already have elapsed by the time the card exists. The next reconcile then
// expires a card nobody had a chance to see, blaming a human who was given no
// window at all.
//
// The fallback covers a request with no recorded openedAt, giving it an
// approximate window rather than treating the missing observation as the zero
// time, which would expire it on sight.
func openedAt(cr *spiceboxv1alpha1.CredentialUpdateRequest) time.Time {
	if cr.Status.OpenedAt != nil {
		return cr.Status.OpenedAt.Time
	}
	return cr.CreationTimestamp.Time
}

// secretChangedSinceOpen reports whether the backing Secret's content hash
// differs from the baseline recorded when the request opened. A Secret that no
// longer exists counts as "unchanged", logged rather than fulfilled: deletion is
// not evidence a human replaced the value, and this reconciler must never guess
// a credential is fixed.
func (r *Reconciler) secretChangedSinceOpen(ctx context.Context, cr *spiceboxv1alpha1.CredentialUpdateRequest) (bool, error) {
	if cr.Status.CredentialSecretRef == nil {
		// Nothing recorded to compare against (shouldn't happen for a
		// genuinely-Open request -- the Open transition above always stamps
		// this first) -- treat as unchanged rather than erroring. LOUDLY: such
		// a request can never Fulfill, so it silently rides out the whole TTL
		// and then reports "nobody updated the credential" for what is really a
		// missing baseline. The sibling Secret-not-found branch below logs its
		// analogue for the same reason.
		log.FromContext(ctx).Info("credentialupdaterequest: Open request has no recorded backing Secret; "+
			"it can never detect a fix and will expire on the idle TTL",
			"credupdate", cr.Namespace+"/"+cr.Name,
			"session", cr.Spec.SessionRef.Namespace+"/"+cr.Spec.SessionRef.Name)
		return false, nil
	}
	nn := types.NamespacedName{Namespace: cr.Status.CredentialSecretRef.Namespace, Name: cr.Status.CredentialSecretRef.Name}
	var sec corev1.Secret
	if err := r.Client.Get(ctx, nn, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("credentialupdaterequest: backing Secret not found while Open; treating as unchanged",
				"credupdate", cr.Namespace+"/"+cr.Name, "secret", nn.String())
			return false, nil
		}
		return false, fmt.Errorf("get backing Secret %s: %w", nn, err)
	}
	return secretContentHash(sec.Data, cr.Status.CredentialSecretKey) != cr.Status.CredentialSecretObservedHash, nil
}

// secretContentHash returns a deterministic SHA-256 digest of the part of a
// Secret's Data that IS the credential, used to detect a genuine content change
// (a human replacing the credential value) as distinct from an unrelated
// metadata/label edit that would merely bump resourceVersion.
//
// key SCOPES the digest, a correctness requirement rather than an optimization.
// The operator projects EVERY one of a userPassthrough session's static
// credentials into ONE per-session Secret keyed by credential name, so "the
// backing Secret changed" is not the same fact as "this credential changed".
// Digesting every key lets an unrelated credential's rewrite flip an unrelated
// OPEN request to Fulfilled, telling the agent to retry over a credential nobody
// touched: it retries into the identical failure having burned an ask, while the
// human still looking at the real card is never told it closed.
//
// An empty key means the whole Secret IS the credential — the type=oauth bundle,
// where any key changing is a real change to that one credential. It is also
// what a request carrying no recorded key has, and whole-Secret hashing is
// exactly the baseline such a request recorded, so the comparison stays coherent
// instead of reading every one of them as instantly changed.
func secretContentHash(data map[string][]byte, key string) string {
	if key != "" {
		// Absent key -> an empty digest, distinct from any present value, so a
		// credential later ADDED under that key still reads as a change.
		var scoped map[string][]byte
		if v, ok := data[key]; ok {
			scoped = map[string][]byte{key: v}
		}
		data = scoped
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeStatus stamps this reconciler's status fields and persists them as a
// merge PATCH of the diff against prior — the object exactly as this reconcile
// read it.
//
// A diff patch rather than a full Status().Update because this status has TWO
// writers: channelsd stamps interactionRef and the CardDelivered condition on
// the same object. A full Update re-serializes every field from a possibly stale
// snapshot, and its resourceVersion precondition turns any interleaving into a
// 409 — which controller-runtime answers by re-running the WHOLE reconcile,
// including refresh.Run, which ROTATES the refresh token, and the live probe.
// Sending only what changed removes both: untouched fields are never re-sent,
// and the patch is resourceVersion-free so it cannot conflict. Same shape and
// reason as agentstatus.WriteOwned. Conditions are never written here, so the
// co-owned array never appears in the patch.
//
// It is also the ONE place every determination is logged. The CR carries an
// ownerReference to the AgentSession, so it is GC'd the moment the session goes
// away, taking the only record of what the determination engine decided with it.
// Without this line, "the agent said the credential was dead and the platform
// refused" leaves no durable trace: no event, no metric, nothing to grep. Every
// exit funnels through here, so one statement covers all of them.
func (r *Reconciler) writeStatus(ctx context.Context, cr, prior *spiceboxv1alpha1.CredentialUpdateRequest,
	phase string, outcome credupdate.Outcome) (ctrl.Result, error) {
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.Phase = phase
	cr.Status.Determination = outcome.Determination
	cr.Status.Reason = outcome.Reason
	log.FromContext(ctx).Info("credentialupdaterequest: determination",
		"credupdate", cr.Namespace+"/"+cr.Name,
		"session", cr.Spec.SessionRef.Namespace+"/"+cr.Spec.SessionRef.Name,
		"origin", cr.Spec.Origin,
		"tool", cr.Spec.ToolName,
		"credential", resolvedCredentialLabel(cr.Status.ResolvedCredential),
		"phase", phase,
		"determination", outcome.Determination,
		"tier", string(outcome.Tier),
		"reason", outcome.Reason)
	if err := r.Client.Status().Patch(ctx, cr, client.MergeFrom(prior)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch CredentialUpdateRequest %s/%s status: %w", cr.Namespace, cr.Name, err)
	}
	return ctrl.Result{}, nil
}

// resolvedCredentialLabel renders a ResolvedCredentialRef for a log field, or
// a legible placeholder when resolution never got that far (the two
// resolution-failure refusals leave it nil, per the field's doc).
func resolvedCredentialLabel(ref *spiceboxv1alpha1.ResolvedCredentialRef) string {
	if ref == nil {
		return "<unresolved>"
	}
	return fmt.Sprintf("%s/%s/%s#%s", ref.IdentityKind, ref.Namespace, ref.Name, ref.Credential)
}

// IsKnownIdentityKind reports whether kind is one of the three identity CR Kind
// spellings ResolvedCredentialRef.IdentityKind documents, the gate this
// reconciler enforces before letting a credential reach Determine. UserIdentity
// and SessionUserIdentity are both user-owned — the latter is the per-session
// projection of the former, same ownership, different CR — while AgentIdentity
// is the agent's own shared credential; see the call site for what makes
// admitting it safe.
//
// Deliberately an ALLOWLIST rather than a blacklist: an empty, mistyped, or
// future-unknown kind fails closed instead of passing through as updatable.
// Recognizing a new identity kind means adding a member, never inverting the
// check to something like `kind != ""`, which would admit any garbage string.
func IsKnownIdentityKind(kind string) bool {
	return kind == spiceboxv1alpha1.IdentityKindAgentIdentity ||
		kind == spiceboxv1alpha1.IdentityKindUserIdentity ||
		kind == spiceboxv1alpha1.IdentityKindSessionUserIdentity
}

// sessionIdentity is what resolveIdentity hands back: the runtime identity to
// resolve credentials against, the identity CR it was projected from, and the
// AgentClass-declared credential remap for THIS request's origin.
//
// A struct rather than a widening tuple: the identity fields are all strings and
// positionally interchangeable, so a transposed pair would compile and silently
// resolve the wrong CR.
type sessionIdentity struct {
	ident credresolve.RuntimeIdentity
	// kind/name/namespace identify the CR ident was projected from
	// ("AgentIdentity", "UserIdentity", "SessionUserIdentity"); they populate
	// ResolvedCredentialRef verbatim. See credupdate.ResolveInput's own doc for
	// why these are the caller's job and never inferred from ident.
	kind      string
	name      string
	namespace string
	// remap is ac.Spec.MCPServers[i].CredentialRemap for the MCPServer this
	// request's Origin names, nil when there is none. It is carried here rather
	// than re-derived at the call site because resolveIdentity is the only place
	// that loads the AgentClass.
	remap map[string]string
	// authFailure is the session's own recorded auth-failure observation for THIS
	// request's Origin, nil when there is none. It is the platform's independent
	// corroboration of the agent's untrusted claim, becoming
	// credupdate.Input.Corroborated at the call site.
	//
	// It rides along for the same reason remap does — resolveIdentity is the only
	// place that loads the AgentSession — and because it MUST come from the
	// session the CR's ownerReference was verified against: a second,
	// independently-resolved Get could drift from that check.
	authFailure *spiceboxv1alpha1.CredentialAuthFailure
}

// resolveIdentity resolves the AgentSession named by cr.Spec.SessionRef to its
// runtime identity, mirroring the effective-identity-mode resolution the runner
// and agentsession's sessionRuntimeIdentity each perform, so this reconciler
// cannot land on a different identity than the one the session is running with.
// A non-nil refusal means "stop, write this determination"; a non-nil error is
// an infrastructure failure the caller should requeue on.
//
// Two checks guard against a cross-tenant confused deputy before the session is
// ever touched. SessionRef.Namespace must equal the CR's own namespace:
// everything else — the MCPServer lookup in ResolveOrigin, the ask-budget List —
// already operates in the CR's namespace, so a mismatched SessionRef would
// silently resolve one namespace's session and identity against another's
// MCPServer and budget. And the CR's ownerReference must point at the resolved
// AgentSession by UID, not merely by name: a same-named session in the right
// namespace with a DIFFERENT UID is not the session that created this request.
func (r *Reconciler) resolveIdentity(ctx context.Context, cr *spiceboxv1alpha1.CredentialUpdateRequest) (
	sessionIdentity, *credupdate.Outcome, error,
) {
	if cr.Spec.SessionRef.Namespace != cr.Namespace {
		return sessionIdentity{}, &credupdate.Outcome{
			Tier:          credupdate.TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
			Reason: fmt.Sprintf("spec.sessionRef names namespace %q, which does not match this request's own "+
				"namespace %q; refusing rather than resolving a cross-namespace session.",
				cr.Spec.SessionRef.Namespace, cr.Namespace),
		}, nil
	}

	sessKey := types.NamespacedName{Namespace: cr.Spec.SessionRef.Namespace, Name: cr.Spec.SessionRef.Name}
	var sess spiceboxv1alpha1.AgentSession
	if getErr := r.Client.Get(ctx, sessKey, &sess); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return sessionIdentity{}, &credupdate.Outcome{
				Tier:          credupdate.TierNone,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
				Reason:        fmt.Sprintf("AgentSession %s was not found, so its identity cannot be resolved.", sessKey),
			}, nil
		}
		return sessionIdentity{}, nil, fmt.Errorf("get AgentSession %s: %w", sessKey, getErr)
	}

	if !ownedBySession(cr, sess.UID) {
		return sessionIdentity{}, &credupdate.Outcome{
			Tier:          credupdate.TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
			Reason: fmt.Sprintf("this request is not owned by AgentSession %s; refusing to resolve an "+
				"unowned session's identity.", sessKey),
		}, nil
	}

	classKey := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}
	var ac spiceboxv1alpha1.AgentClass
	if getErr := r.Client.Get(ctx, classKey, &ac); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return sessionIdentity{}, &credupdate.Outcome{
				Tier:          credupdate.TierNone,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
				Reason:        fmt.Sprintf("AgentClass %s was not found, so the session's identity cannot be resolved.", classKey),
			}, nil
		}
		return sessionIdentity{}, nil, fmt.Errorf("get AgentClass %s: %w", classKey, getErr)
	}

	// The AgentClass is loaded here and nowhere else, so the per-MCPServer
	// credentialRemap is picked up in the same pass rather than re-Getting it.
	remap := credentialRemapForOrigin(&ac, cr.Spec.Origin)

	// Same for the session's own auth-failure observation: sess is in hand and
	// ownership-verified above. Matched on cr.Spec.Origin, the SAME vocabulary
	// the runner keys its observations by, so no translation step can go wrong —
	// an observation for a different origin is a different credential and must
	// not corroborate this request.
	authFailure := spiceboxv1alpha1.FindCredentialAuthFailure(sess.Status.CredentialAuthFailures, cr.Spec.Origin)

	// ask|dynamic resolves through status.effectiveIdentityMode once the
	// initiating user's choice has been made, else the provisional "agent"
	// default a freshly-booted ask|dynamic session runs on; static modes
	// pass through unchanged. Byte-for-byte the same switch internal/cmd/runner and
	// sessionRuntimeIdentity use.
	mode := ac.Spec.IdentityMode
	switch mode {
	case spiceboxv1alpha1.IdentityModeAsk, spiceboxv1alpha1.IdentityModeDynamic:
		if sess.Status.EffectiveIdentityMode != "" {
			mode = sess.Status.EffectiveIdentityMode
		} else {
			mode = spiceboxv1alpha1.IdentityModeAgent
		}
	}

	if mode == spiceboxv1alpha1.IdentityModeUserPassthrough {
		suidKey := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
		var suid spiceboxv1alpha1.SessionUserIdentity
		if getErr := r.Client.Get(ctx, suidKey, &suid); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return sessionIdentity{}, &credupdate.Outcome{
					Tier:          credupdate.TierNone,
					Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
					Reason:        fmt.Sprintf("SessionUserIdentity %s was not found, so its identity cannot be resolved.", suidKey),
				}, nil
			}
			return sessionIdentity{}, nil, fmt.Errorf("get SessionUserIdentity %s: %w", suidKey, getErr)
		}
		return sessionIdentity{
			ident:       credresolve.RuntimeIdentityFromSessionUserIdentity(&suid),
			kind:        "SessionUserIdentity",
			name:        suid.Name,
			namespace:   suid.Namespace,
			remap:       remap,
			authFailure: authFailure,
		}, nil, nil
	}

	// agent mode (static agent/unset, or ask|dynamic provisional/resolved-agent).
	identityName := sess.Spec.AgentIdentity
	if identityName == "" {
		identityName = ac.Spec.AgentIdentity
	}
	if identityName == "" {
		// No class/session-level identity configured at all: nothing to
		// resolve against. ResolveOrigin refuses NoCredential on its own for
		// a zero-valued RuntimeIdentity (it resolves no credentials), so
		// there is no need to duplicate that refusal here -- but this is
		// still worth a log line: a silent no-op return here previously left
		// no grep-able trace of why an agent-mode session had no identity.
		log.FromContext(ctx).Info("credentialupdaterequest: no AgentIdentity configured for this session/class; "+
			"ResolveOrigin will refuse NoCredential", "session", sess.Namespace+"/"+sess.Name, "class", sess.Spec.Class)
		return sessionIdentity{remap: remap, authFailure: authFailure}, nil, nil
	}

	aiKey := types.NamespacedName{Namespace: sess.Namespace, Name: identityName}
	var ai spiceboxv1alpha1.AgentIdentity
	if getErr := r.Client.Get(ctx, aiKey, &ai); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return sessionIdentity{}, &credupdate.Outcome{
				Tier:          credupdate.TierNone,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
				Reason:        fmt.Sprintf("AgentIdentity %s was not found, so its identity cannot be resolved.", aiKey),
			}, nil
		}
		return sessionIdentity{}, nil, fmt.Errorf("get AgentIdentity %s: %w", aiKey, getErr)
	}
	return sessionIdentity{
		ident:       credresolve.RuntimeIdentityFromAgentIdentity(&ai),
		kind:        "AgentIdentity",
		name:        ai.Name,
		namespace:   ai.Namespace,
		remap:       remap,
		authFailure: authFailure,
	}, nil, nil
}

// credentialRemapForOrigin returns the AgentClass-declared credentialRemap for
// the MCPServer named by origin ("mcpserver/<CR name>"), or nil when the origin
// is not an MCPServer origin, names a server this class does not reference, or
// that reference declares no remap.
//
// This exists because the two halves of credential naming live in different
// places: mcpkind.SetupRequirements suggests the name the TOOL declares, while
// the identity's credentials are keyed by the POST-remap name. Resolving the
// former against the latter finds nothing (or, worse, the wrong credential).
// Matching is on Ref -- the MCPServer CR name -- because that is exactly what
// MCPTool.Origin() emits ("mcpserver/" + cr.Name), NOT the LLM-facing prefix.
func credentialRemapForOrigin(ac *spiceboxv1alpha1.AgentClass, origin string) map[string]string {
	kind, name, ok := credupdate.SplitOrigin(origin)
	if !ok || kind != "mcpserver" {
		return nil
	}
	for i := range ac.Spec.MCPServers {
		if ac.Spec.MCPServers[i].Ref == name {
			return ac.Spec.MCPServers[i].CredentialRemap
		}
	}
	return nil
}

// peerRequests lists every OTHER CredentialUpdateRequest in cr's namespace --
// cr itself excluded once, here, so neither caller has to remember to skip it
// (and so neither can forget: counting yourself against your own budget, or
// collapsing onto yourself, are both silent wedges).
//
// Namespace-scoped List rather than a Get: there is no index from credential to
// request, and a request's name is caller-chosen (the meta tool that creates it)
// rather than derived from the credential, so there is no key to Get by.
// Cardinality per namespace is small -- bounded by how many sessions share it --
// and the read is cache-backed.
func (r *Reconciler) peerRequests(ctx context.Context, cr *spiceboxv1alpha1.CredentialUpdateRequest) (
	[]spiceboxv1alpha1.CredentialUpdateRequest, error) {
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := r.Client.List(ctx, &list, client.InNamespace(cr.Namespace)); err != nil {
		return nil, fmt.Errorf("list CredentialUpdateRequests in %q: %w", cr.Namespace, err)
	}
	out := make([]spiceboxv1alpha1.CredentialUpdateRequest, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Name == cr.Name {
			continue
		}
		out = append(out, list.Items[i])
	}
	return out, nil
}

// resolvedTo reports whether item is a request about the credential ref names.
// A request that never got as far as resolving one is not about any credential
// and matches nothing.
//
// This is the ASK BUDGET's match, and deliberately not the collapse election's
// (see credentialHandle). The two ask different questions of the same rows and
// need different keys:
//
//   - The budget counts a session's own prior asks about a credential,
//     including the ones that were REFUSED -- and a refused request records
//     status.resolvedCredential but never a backing Secret, because only the
//     Open transition stamps one. Its identity coordinates are the only handle
//     it has, and within one session they are stable.
//   - Collapse only ever considers OPEN peers, which always recorded a Secret,
//     and it must match ACROSS sessions -- where identity coordinates stop
//     identifying the credential and start identifying the projection.
func resolvedTo(item *spiceboxv1alpha1.CredentialUpdateRequest, ref spiceboxv1alpha1.ResolvedCredentialRef) bool {
	return item.Status.ResolvedCredential != nil && sameCredential(*item.Status.ResolvedCredential, ref)
}

// credentialHandle is the canonical answer to "which credential is this request
// about": the identity's OWNERSHIP class, plus the backing Secret coordinates
// (namespace, name, key) the credential's value actually lives at.
//
// The Secret coordinates replaced the identity CR's own for the collapse
// election because the latter name the PROJECTION, not the credential. A
// SessionUserIdentity is named after its own session (agentsession's
// BuildSessionUserIdentity), so two sessions belonging to the same person and
// resolving the IDENTICAL master Secret carried two different Names and could
// never be seen as one ask. Collapsing was therefore structurally impossible
// for every user-owned credential -- which, since Determine refuses agent-owned
// oauth outright, is every oauth card the feature raises. N sessions on one
// dead connection meant N DMs to one human. The Secret coordinates are the same
// string for both, because they ARE the same credential.
//
// IdentityKind stays in the handle. Ownership decides who is shown the card,
// which gate authorizes the click, and which object the replacement is written
// to, so an agent-owned and a user-owned credential must never be treated as
// one ask even if they somehow named a single Secret.
type credentialHandle struct {
	identityKind string
	// namespace/name/key are CredentialSource's coordinates -- for a
	// type=static credential the key is what distinguishes it from every other
	// credential sharing the same projected Secret; for type=oauth the whole
	// Secret is the credential and the key is empty.
	namespace string
	name      string
	key       string
}

// handleForSource builds the handle for the credential this reconcile just
// resolved, from the descriptor ResolveOrigin returned.
func handleForSource(identityKind string, src spiceboxv1alpha1.CredentialSource) credentialHandle {
	return credentialHandle{identityKind: identityKind, namespace: src.Namespace, name: src.Name, key: src.Key}
}

// handleForRequest reads back the handle a peer RECORDED, and reports false
// when it has none.
//
// Only the Open transition stamps CredentialSecretRef/Key, so "no handle" means
// the request never opened a card -- it resolved nothing, was refused, or is a
// follower -- and it is about no credential this election may match. An Open
// peer somehow missing one is not silently swallowed: that request's own
// reconcileOpen logs it loudly ("can never detect a fix and will expire on the
// idle TTL"), which is the same anomaly seen from the side that can act on it.
func handleForRequest(item *spiceboxv1alpha1.CredentialUpdateRequest) (credentialHandle, bool) {
	if item.Status.ResolvedCredential == nil || item.Status.CredentialSecretRef == nil {
		return credentialHandle{}, false
	}
	return credentialHandle{
		identityKind: item.Status.ResolvedCredential.IdentityKind,
		namespace:    item.Status.CredentialSecretRef.Namespace,
		name:         item.Status.CredentialSecretRef.Name,
		key:          item.Status.CredentialSecretKey,
	}, true
}

// askIsSpent reports whether a request in this phase has SPENT one of its
// session's asks for the credential it resolved.
//
// An ask is spent the moment this reconciler DETERMINES it, because that is the
// moment it starts costing a human something:
//
//   - Open -- a card is (or is about to be) in front of a person. The obvious
//     case.
//   - Collapsed -- this ask attached itself to a card ALREADY in front of a
//     person. It consumed that person's attention just as surely as its own
//     card would have, which is exactly why the follower is entitled to settle
//     with the canonical's answer rather than being told nothing happened.
//   - Fulfilled / Refused / Expired -- concluded, one way or another.
//
// Only a request still awaiting its first determination ("" or the explicit
// Pending stamp) is unspent: nothing has been decided about it, and it may yet
// be refused outright without ever costing anybody anything.
//
// COUPLED: that unspent set is exactly Reconcile's phase-gate fall-through arm
// -- the phases that still route into the determination pipeline -- and the two
// must move together. A new undetermined phase added to the gate but not here
// would charge the session an ask for a request that decided nothing.
//
// # Why this is NOT IsCredentialUpdateRequestTerminal
//
// Counting only terminal phases was a live hole. Collapsed is deliberately
// non-terminal (the blocked agent must keep waiting rather than be told its ask
// was settled when nothing has been decided), so under a terminal-only count a
// session could pile ask after ask onto ONE live card and spend nothing --
// bounded only by how fast its tool calls time out. Two sessions taking turns
// as canonical and follower could then trade roles indefinitely and neither
// would ever exhaust its two asks, which is precisely the evasion the budget
// exists to prevent. Open was the same hole one step earlier: a session whose
// own card is live got unlimited further asks against it, so "at most two asks"
// was not what the budget actually enforced.
//
// # Why a denylist of the two UNDETERMINED phases, not an allowlist
//
// The repo's usual shape is an allowlist, because for those predicates
// "recognized" is the permissive answer and an unknown value must fail closed.
// Here the polarity is reversed: COUNTING is the closed answer. An unrecognized
// phase is one Reconcile's default arm treats as already decided and will never
// move again, so it is a spent ask by any reading; leaving it uncounted would
// hand an agent a free ask for every phase typo. So the two undetermined phases
// are enumerated and everything else counts.
func askIsSpent(phase string) bool {
	switch phase {
	case "", spiceboxv1alpha1.CredentialUpdateRequestPhasePending:
		return false
	default:
		return true
	}
}

// budgetExceeded reports whether session has already spent at least budgetLimit
// asks about ref -- see askIsSpent for what "spent" means, and why it is
// broader than "terminal".
//
// It counts live objects rather than a persisted counter: each CR carries an
// owner-ref to its session, so it lives exactly as long as the session does,
// and every prior ask for this exact credential is therefore already sitting in
// peers to be counted. A pure function over the slice, so its whole contract is
// visible without reasoning about what was read when.
//
// The count is charged to item.Spec.SessionRef -- the session that RAISED the
// ask -- and never to whoever's card it ended up riding on. A follower spends
// its OWN session's budget, not the canonical's: the canonical's session asked
// once and must not be charged for every other session that piled onto its
// card, or one shared dead credential would exhaust the budget of the one
// session actually holding the card.
func budgetExceeded(peers []spiceboxv1alpha1.CredentialUpdateRequest,
	session spiceboxv1alpha1.NamespacedRef, ref spiceboxv1alpha1.ResolvedCredentialRef) bool {
	count := 0
	for i := range peers {
		item := &peers[i]
		if item.Spec.SessionRef != session {
			continue // the budget is charged per SESSION, not per credential alone
		}
		if !askIsSpent(item.Status.Phase) {
			continue // still awaiting its first determination: nothing spent yet
		}
		if !resolvedTo(item, ref) {
			continue
		}
		count++
	}
	return count >= budgetLimit
}

// sameCredential compares the fields that identify a credential (not
// ProviderID, which is display metadata only).
func sameCredential(a, b spiceboxv1alpha1.ResolvedCredentialRef) bool {
	return a.IdentityKind == b.IdentityKind && a.Namespace == b.Namespace &&
		a.Name == b.Name && a.Credential == b.Credential
}

// collapsedRecheckInterval bounds how long a follower may keep waiting after
// its canonical has settled. It is a BACKSTOP, not the mechanism: a follower
// re-reads its canonical on every reconcile, and this only covers the case
// where nothing else woke it. Deliberately short relative to the idle TTL, so
// a follower's total delay is dominated by the human, not by this poll.
const collapsedRecheckInterval = 15 * time.Second

// canonicalOpenRequestFor returns the request that already holds a live card
// for ref, or nil when none does. That request is the CANONICAL one; cr
// becomes its follower.
//
// # How the election stays deterministic and cycle-free
//
// The hazard this must rule out is two requests landing in the same reconcile
// window and each electing the OTHER: two followers, no card, and both agents
// blocked until their wait windows elapse — strictly worse than the duplicate
// cards collapsing exists to remove, because at least a duplicate gets
// somebody asked.
//
// Properties (1) and (2) ALONE make that unrepresentable, and they are the two
// lines nothing here may delete:
//
//  1. A candidate must ALREADY be in phase Open. Open is a committed,
//     persisted state that only this reconciler writes, and only at the very
//     end of a completed determination. So candidacy is a fact about state
//     that is already durable — never about a decision in flight — and the
//     answer does not depend on who is reconciling right now.
//  2. A follower NEVER reaches Open. It is written Collapsed, and Reconcile's
//     phase gate routes an Open request to reconcileOpen — which only ever
//     writes Fulfilled or Expired — so an Open request can never re-enter the
//     determination pipeline and become a follower. A canonical therefore
//     cannot be a follower, and no cycle of any length can form. The
//     CollapsedInto skip below makes that structural rather than merely implied
//     by (1), so a future path that somehow left a follower Open still cannot
//     start a chain.
//
// Two further properties decide only WHICH request is canonical, never how
// many:
//
//  3. Of two requests created together, one reconciles first and Opens; the
//     second observes that committed Open and follows. Reconciles are
//     serialized per controller (MaxConcurrentReconciles defaults to 1), and
//     the shipped Deployment is `replicas: 1` with `strategy: Recreate`
//     (config/manager/deployment.yaml), so exactly one process is reconciling
//     at a time. That is where the single-writer property comes from —
//     explicitly NOT from leader election, which `--leader-elect` leaves off by
//     default (internal/cmd/operator/main.go).
//  4. Should more than one Open peer exist anyway (two cards opened before
//     either observed the other — a redeploy overlap, or a future
//     MaxConcurrentReconciles > 1), the canonical is the lexicographically
//     smallest NAME among them. Names are unique within a namespace, so that
//     is a total order every follower computes identically: they all attach to
//     the SAME canonical rather than splitting across two, which is what keeps
//     the follower set derivable by a single list.
//
// So the worst case if (3) is ever violated is two CANONICALS — two cards for
// one credential, the graceful degradation collapsing exists to reduce — never
// two followers with no card at all.
//
// peers must already exclude the requesting object (peerRequests does that), so
// there is no self-collapse case to handle here.
func canonicalOpenRequestFor(peers []spiceboxv1alpha1.CredentialUpdateRequest,
	want credentialHandle) *spiceboxv1alpha1.CredentialUpdateRequest {
	var canonical *spiceboxv1alpha1.CredentialUpdateRequest
	for i := range peers {
		item := &peers[i]
		if item.Status.Phase != spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen {
			continue // only a LIVE card can absorb another ask -- property (1)
		}
		if item.Status.CollapsedInto != nil {
			continue // never chain onto a follower -- property (2)
		}
		// Keyed on the CREDENTIAL -- the backing Secret coordinates plus
		// ownership -- not on "some other request is open" and not on the
		// identity CR: one identity commonly holds several credentials, and a
		// second, genuinely-different dead token silently swallowed here would
		// leave that agent with no card and no human ever asked about it. See
		// credentialHandle for why the identity CR's own name is the wrong
		// coordinate in the other direction too.
		got, ok := handleForRequest(item)
		if !ok || got != want {
			continue
		}
		if canonical == nil || item.Name < canonical.Name {
			canonical = item // lowest name wins -- property (4)
		}
	}
	return canonical
}

// collapseOnto records that cr is waiting on canonical's card, and publishes
// nothing.
//
// Everything that would make cr LOOK delivered is deliberately left untouched:
// InteractionRef and the CardDelivered condition are stamped only by channelsd,
// and only when a card was really shown to somebody. A follower has no card, so
// stamping either would make slice 1's expiry branch — which reads exactly
// those fields to say "never delivered to a human" rather than "nobody acted" —
// start lying about a human who was never asked. CredentialSecretRef is left
// unset for the same reason: it is the baseline the unpark Secret watch
// compares against for a card, and this request has none to unpark.
func (r *Reconciler) collapseOnto(ctx context.Context, cr, prior,
	canonical *spiceboxv1alpha1.CredentialUpdateRequest) (ctrl.Result, error) {
	cr.Status.CollapsedInto = &spiceboxv1alpha1.NamespacedRef{Namespace: canonical.Namespace, Name: canonical.Name}
	// Logged separately from writeStatus' determination line, which has no
	// vocabulary for the canonical: "one card for five sessions" is only
	// auditable if the other four say, in the log, which card they attached to.
	log.FromContext(ctx).Info("credentialupdaterequest: an open card already exists for this credential; "+
		"collapsing onto it instead of asking a second human",
		"credupdate", cr.Namespace+"/"+cr.Name,
		"session", cr.Spec.SessionRef.Namespace+"/"+cr.Spec.SessionRef.Name,
		"collapsedInto", canonical.Namespace+"/"+canonical.Name,
		"canonicalSession", canonical.Spec.SessionRef.Namespace+"/"+canonical.Spec.SessionRef.Name,
		"credential", resolvedCredentialLabel(cr.Status.ResolvedCredential))
	// "A human has ALREADY been asked" is a claim about publication, and this
	// reconciler does not publish: it writes Open, and channelsd — a separate
	// process — puts the card in front of somebody, with its own silent skip
	// paths (no InputChannel, no started-by subject, no ResolvedCredential, its
	// signing key missing). A canonical that is Open but not yet (or never)
	// delivered means NOBODY has been asked, and saying otherwise here is slice
	// 1's honesty bug wearing a third costume. canonical.Status.InteractionRef
	// is the same discriminator the expiry branch uses — stamped by channelsd in
	// the SAME patch as CardDelivered=True — so the two can never disagree.
	//
	// This is a SNAPSHOT taken at collapse time, and deliberately not refreshed
	// when the canonical is later delivered: the follower's Collapsed arm returns
	// before any status write, so re-rendering it would mean a status PUT per
	// follower per recheck. The agent's live answer comes from the meta tool,
	// which re-reads the canonical at give-up time (canonicalDelivered).
	reason := "A human has already been asked to replace this exact credential, for another session. " +
		"This request is waiting on that one rather than asking a second person; keep waiting."
	if canonical.Status.InteractionRef == "" {
		reason = "Another session has already raised a request to replace this exact credential, and this request " +
			"is waiting on that one rather than asking a second person -- though that request has not been " +
			"confirmed delivered to a human yet. Keep waiting."
	}
	// TierNone: no card is opened for THIS request. The reason is
	// platform-authored like every other, and deliberately does not name the
	// canonical request — the agent has no use for another session's object
	// name, and status.collapsedInto already records it for operators.
	_, err := r.writeStatus(ctx, cr, prior, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, credupdate.Outcome{
		Tier:          credupdate.TierNone,
		Determination: spiceboxv1alpha1.CredentialUpdateDeterminationCollapsed,
		Reason:        reason,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	// Start the recheck loop HERE rather than relying on the watch event this
	// very Status().Update generates. A follower's only self-scheduled wakeups
	// come from the Collapsed arm of Reconcile, and that arm is not reached
	// until something enqueues it: if this one event is dropped (an informer
	// resync gap, a restart between the write and the delivery), the next
	// guaranteed wakeup is the manager's SyncPeriod -- 10h, hours past any wait
	// window. Returning the interval makes the loop start unconditionally, from
	// the write that created the follower.
	return ctrl.Result{RequeueAfter: collapsedRecheckInterval}, nil
}

// canonicalFor re-reads the request cr collapsed onto. It returns (nil, nil)
// when there is nothing to read: no recorded pointer, or the canonical was
// DELETED (its session ended, taking its owner-ref'd request with it). Both
// mean the card cr was waiting on is gone, and continuing to wait on it is the
// "follower that never unblocks" failure.
//
// It hands back the OBJECT rather than a live/not-live bool because the
// canonical's phase is not merely a gate: once it is terminal, the canonical IS
// this follower's answer, and its phase, determination and delivery mark are
// all inputs to settleWithCanonical.
func (r *Reconciler) canonicalFor(ctx context.Context, cr *spiceboxv1alpha1.CredentialUpdateRequest) (
	*spiceboxv1alpha1.CredentialUpdateRequest, error) {
	logger := log.FromContext(ctx)
	if cr.Status.CollapsedInto == nil {
		// Phase says Collapsed but nothing records onto what. collapseOnto
		// writes both in one status update, so this should be unreachable --
		// logged rather than silently treated as ordinary, because the request
		// is about to be re-determined from scratch and an operator reading
		// "why did this ask twice" deserves the trace.
		logger.Info("credentialupdaterequest: phase is Collapsed but status.collapsedInto is empty; "+
			"re-determining this request from scratch", "credupdate", cr.Namespace+"/"+cr.Name)
		return nil, nil
	}
	nn := types.NamespacedName{Namespace: cr.Status.CollapsedInto.Namespace, Name: cr.Status.CollapsedInto.Name}
	var canonical spiceboxv1alpha1.CredentialUpdateRequest
	if err := r.Client.Get(ctx, nn, &canonical); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("credentialupdaterequest: the request this one collapsed onto no longer exists",
				"credupdate", cr.Namespace+"/"+cr.Name, "collapsedInto", nn.String())
			return nil, nil
		}
		return nil, fmt.Errorf("get canonical CredentialUpdateRequest %s: %w", nn, err)
	}
	return &canonical, nil
}

// settleWithCanonical is the propagation step: the request this one was waiting
// on reached a terminal decision, so this follower reaches the SAME one, now.
//
// This is what makes a follower safe to create at all. A follower publishes
// nothing, probes nothing, and carries no deadline of its own, so nothing else
// in the system will ever move it. If it did not settle here, its agent would
// block its entire wait window and then report a timeout for a credential a
// human really did replace -- failure mode 1, and the third variant this
// project would have shipped of "the system blames a human who wasn't asked".
//
// Phase and determination are copied VERBATIM. The two requests are about the
// identical credential (sameCredential is what elected the canonical in the
// first place), so the canonical's answer IS this request's answer; a follower
// handed some generic "the other one finished" determination would leave its
// agent unable to tell a refusal from an expiry, or a self-heal from a denial.
// Only phases IsCredentialUpdateRequestTerminal admits reach here (the caller
// gates on it), so an unrecognized phase on the canonical can never be
// laundered into a follower.
//
// The REASON is rewritten rather than copied -- see followerReason. That is not
// polish: the canonical's reason is written from the point of view of a request
// that HAD a card, and reusing it verbatim on a request no card was ever
// published for is slice 1's honesty bug in a new costume.
//
// status.collapsedInto is deliberately LEFT SET. It is the only durable record
// of whose answer this outcome came from, and an operator reading a settled
// follower needs it to reconstruct "one card, five sessions".
func (r *Reconciler) settleWithCanonical(ctx context.Context, cr, prior,
	canonical *spiceboxv1alpha1.CredentialUpdateRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if canonical.Status.Determination == "" {
		// Never expected -- every phase this reconciler writes goes through
		// writeStatus with an Outcome. Logged rather than papered over with a
		// substitute: inventing a determination would put a verdict on the
		// record that nothing ever computed.
		logger.Info("credentialupdaterequest: the request this one collapsed onto settled carrying no "+
			"determination; this request settles on the phase alone",
			"credupdate", cr.Namespace+"/"+cr.Name,
			"collapsedInto", canonical.Namespace+"/"+canonical.Name,
			"canonicalPhase", canonical.Status.Phase)
	}
	// Logged separately from writeStatus' determination line, which has no
	// vocabulary for the canonical: "one card settled five sessions" is only
	// auditable if each follower says, in the log, whose answer it took.
	logger.Info("credentialupdaterequest: the request this one was waiting on settled; settling with it",
		"credupdate", cr.Namespace+"/"+cr.Name,
		"session", cr.Spec.SessionRef.Namespace+"/"+cr.Spec.SessionRef.Name,
		"collapsedInto", canonical.Namespace+"/"+canonical.Name,
		"canonicalSession", canonical.Spec.SessionRef.Namespace+"/"+canonical.Spec.SessionRef.Name,
		"phase", canonical.Status.Phase)
	return r.writeStatus(ctx, cr, prior, canonical.Status.Phase, credupdate.Outcome{
		Tier:          credupdate.TierNone,
		Determination: canonical.Status.Determination,
		Reason:        followerReason(canonical),
	})
}

// followerReason renders a settled canonical's outcome from the FOLLOWER's
// point of view. It is the one thing propagation may not copy verbatim.
//
// One rule constrains every branch: this request was never delivered a card, so
// nothing here may imply a human was shown something for IT and failed to act.
// The Expired branches are where that bites hardest -- the canonical's own
// wording ("Nobody updated the credential before the wait window elapsed")
// reads, on a request nobody was ever shown, as an accusation against a person
// who was never asked. That is precisely the class of defect slice 1 built the
// interactionRef discriminator for, and that slice 3 had to repair twice.
//
// Refused is the opposite case: there the canonical's reason is the real,
// platform-authored "why" (never the agent's own `why` text), and a follower
// given a generic stand-in would report something no human ever decided. So it
// is carried through, with a suffix saying whose ask it answers.
func followerReason(canonical *spiceboxv1alpha1.CredentialUpdateRequest) string {
	// Common to every branch, and the clause that keeps all of them honest: no
	// human was ever shown anything on this request's behalf.
	const noCard = "No card was ever shown for this request"

	switch canonical.Status.Phase {
	case spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled:
		return "The credential was updated: a human replaced it in response to the request another session " +
			"raised for this same credential, which this request was waiting on. Retry your call."
	case spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused:
		if canonical.Status.Reason == "" {
			return "The request this one was waiting on -- raised by another session for this same credential -- " +
				"was refused. " + noCard + "."
		}
		return canonical.Status.Reason +
			" (That decision was made on the request another session raised for this same credential, which " +
			"this request was waiting on. " + noCard + ".)"
	case spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired:
		if canonical.Status.InteractionRef != "" {
			// A human WAS shown the shared card -- just not for this request. Say
			// exactly that, and say nothing about what they did or did not do.
			return noCard + ": it was waiting on the credential-update card another session raised for this " +
				"same credential, and that card's wait window elapsed without the credential being replaced."
		}
		return noCard + ": it was waiting on the request another session raised for this same credential, and " +
			"that request was never delivered to a human either, so nobody was ever asked before the wait " +
			"window elapsed."
	default:
		// Unreachable -- the caller gates on IsCredentialUpdateRequestTerminal.
		// Kept, and deliberately claiming nothing at all about a human, so a
		// future terminal phase added without updating this function degrades to
		// a vague-but-true sentence rather than a confident lie.
		return "The request this one was waiting on -- raised by another session for this same credential -- " +
			"reached a decision. " + noCard + "."
	}
}

// namespacedRefLabel renders a NamespacedRef for a log field, nil-safe.
func namespacedRefLabel(ref *spiceboxv1alpha1.NamespacedRef) string {
	if ref == nil {
		return "<none>"
	}
	return ref.Namespace + "/" + ref.Name
}

// canonicalPhaseLabel renders a possibly-absent canonical's phase for a log
// field, so the "re-determining from scratch" line distinguishes a canonical
// that was DELETED from one sitting in an unexpected phase.
func canonicalPhaseLabel(cr *spiceboxv1alpha1.CredentialUpdateRequest) string {
	if cr == nil {
		return "<gone>"
	}
	if cr.Status.Phase == "" {
		return "<empty>"
	}
	return cr.Status.Phase
}

// ownedBySession reports whether cr carries an ownerReference to a resource
// with sessionUID -- i.e. whether cr is genuinely owned by the AgentSession
// spec.sessionRef claims to name, not merely a same-namespace/same-name
// coincidence. UID (not Kind/Name) is the check: it is the one field a
// caller cannot forge by naming an arbitrary session that happens to share a
// name with one it doesn't own.
func ownedBySession(cr *spiceboxv1alpha1.CredentialUpdateRequest, sessionUID types.UID) bool {
	if sessionUID == "" {
		return false
	}
	for _, o := range cr.OwnerReferences {
		if o.UID == sessionUID {
			return true
		}
	}
	return false
}

// attemptRefresh runs the OAuth refresh grant when src's Secret carries a
// refresh_token, recording RefreshRan/RefreshOK/RefreshErr on in. Never asks
// a human for what the machine can do first: this always precedes the live
// probe. A missing Secret is not an error here -- there is simply nothing to
// refresh -- but any other Secret-read failure is a genuine infrastructure
// problem the caller should requeue on.
//
// On success it also invalidates the broker's cache for this Secret.
// r.Broker caches oauth/static resolutions with a ZERO (never-expires) TTL
// (pkg/platform/identity/broker/inproc), so without this the very next probe would
// resolve the PRE-refresh token straight from cache and "SelfHealed" would
// be a lie: the agent's "retry your call" would hit the same dead token.
// Mirrors internal/cmd/runner/main.go's SetReauth callback, which invalidates for the
// identical reason before its own post-401 re-resolve.
func (r *Reconciler) attemptRefresh(ctx context.Context, in *credupdate.Input, src spiceboxv1alpha1.CredentialSource) error {
	logger := log.FromContext(ctx)

	var sec corev1.Secret
	if getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: src.Name}, &sec); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			logger.Info("credentialupdaterequest: oauth Secret not found; skipping refresh attempt",
				"namespace", src.Namespace, "name", src.Name)
			return nil
		}
		return fmt.Errorf("get oauth Secret %s/%s: %w", src.Namespace, src.Name, getErr)
	}
	if len(sec.Data["refresh_token"]) == 0 {
		// No refresh_token: there is nothing to refresh, so Determine will run
		// on the probe alone. Logged for the same reason the NotFound branch
		// above is -- "why did an oauth credential never get a refresh attempt"
		// is otherwise unanswerable from logs.
		logger.Info("credentialupdaterequest: oauth Secret carries no refresh_token; skipping refresh attempt",
			"namespace", src.Namespace, "name", src.Name)
		return nil
	}

	cred, err := credresolve.AgentCredentialFromSource(src)
	if err != nil {
		// Mirrors the NotFound/no-refresh_token branches above: this is a
		// "could not even attempt it" outcome, not a failed attempt, so
		// RefreshRan stays false and Determine runs on the probe alone.
		logger.Info("credentialupdaterequest: could not build a resolvable credential for refresh; skipping refresh attempt",
			"namespace", src.Namespace, "name", src.Name, "err", err.Error())
		return nil
	}

	in.RefreshRan = true
	if rerr := refresh.Run(ctx, r.Client, src.Namespace, cred); rerr != nil {
		in.RefreshErr = rerr.Error()
		logger.Info("credentialupdaterequest: oauth refresh attempt failed",
			"namespace", src.Namespace, "name", src.Name, "err", rerr.Error())
		return nil
	}
	in.RefreshOK = true
	logger.Info("credentialupdaterequest: oauth refresh attempt succeeded",
		"namespace", src.Namespace, "name", src.Name)
	if ierr := r.Broker.InvalidateSecret(src.Namespace, src.Name); ierr != nil {
		logger.Info("credentialupdaterequest: InvalidateSecret after refresh errored",
			"namespace", src.Namespace, "name", src.Name, "err", ierr.Error())
	}
	return nil
}

// probeCredential resolves desc's current value through the broker and
// live-verifies it against prov. It invalidates the broker's cache for this
// Secret FIRST, for the same never-expires-TTL reason documented on
// attemptRefresh: without it, an oauth credential whose access_token expired
// (but whose refresh just failed for a non-invalid_grant reason, e.g. an IdP
// outage) would have its STALE, ALREADY-EXPIRED token served from cache and
// probed -- likely a definitive 401, opening a card ("RejectedVerified") for
// a credential a working refresh would have healed on its own. Invalidating
// first makes Resolve re-read the Secret, hit ErrExpired, retry the JIT
// refresh, and fail closed to an honest VerifyIndeterminate instead.
//
// A broker resolution failure (Secret deleted out from under us, a dead
// refresh loop, ...) is logged and mapped to VerifyIndeterminate rather than
// returned as a reconcile error: it is exactly the kind of "could not
// confirm" evidence VerifyIndeterminate exists to carry, and Determine
// already treats it as a soft-warn, never-block signal.
func (r *Reconciler) probeCredential(ctx context.Context, prov *provider.Provider,
	desc spiceboxv1alpha1.CredentialDescriptor) (builtins.VerifyStatus, string) {
	logger := log.FromContext(ctx)

	if ierr := r.Broker.InvalidateSecret(desc.Source.Namespace, desc.Source.Name); ierr != nil {
		logger.Info("credentialupdaterequest: InvalidateSecret before probe errored",
			"namespace", desc.Source.Namespace, "name", desc.Source.Name, "err", ierr.Error())
	}

	res, err := r.Broker.Resolve(ctx, broker.Request{Credentials: []spiceboxv1alpha1.CredentialDescriptor{desc}})
	if err != nil {
		logger.Info("credentialupdaterequest: broker resolve failed before probe; treating as indeterminate",
			"namespace", desc.Source.Namespace, "name", desc.Source.Name, "err", err.Error())
		return builtins.VerifyIndeterminate, "could not resolve the credential's current value: " + err.Error()
	}

	value := builtins.StoreValue{Bearer: extractValue(res, desc.Inject)}
	vr := builtins.VerifyCredential(ctx, prov, value)
	return vr.Status, vr.Detail
}

// extractValue pulls the bare token out of a broker.Resolution, stripping a
// header injection's ValuePrefix (e.g. "Bearer ") so the probe always
// receives the raw token bytes regardless of which injection shape the
// descriptor uses.
func extractValue(res broker.Resolution, inject spiceboxv1alpha1.CredentialInjection) string {
	switch {
	case inject.EnvVar != "":
		return res.EnvVars[inject.EnvVar]
	case inject.Header != nil:
		return strings.TrimPrefix(res.HTTPHeaders[inject.Header.Name], inject.Header.ValuePrefix)
	default:
		return ""
	}
}

// SetupWithManager wires the controller into mgr.
//
// The second CredentialUpdateRequest watch is NOT redundant with For(): For()
// enqueues the object that CHANGED, this one enqueues that object's FOLLOWERS.
// It is the mechanism by which a settled card settles every session riding on
// it. Without it a follower's only route out of Collapsed is the
// collapsedRecheckInterval poll -- a backstop -- and its agent keeps waiting
// after a human has already acted.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.CredentialUpdateRequest{}).
		Watches(&spiceboxv1alpha1.CredentialUpdateRequest{},
			handler.EnqueueRequestsFromMapFunc(r.mapCanonicalToFollowers)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToRequests)).
		Complete(r)
}

// mapCanonicalToFollowers re-enqueues every request still waiting on the
// changed one -- the canonical->followers half of propagation.
//
// Deliberately NOT filtered on the canonical's own phase. It must fire on
// DELETE too (where the handler is handed the object as it last was), and that
// is the case a phase filter would miss entirely: a canonical GC'd with its
// session leaves its followers pointing at a name that no longer resolves, and
// they need waking to re-determine their own asks.
//
// The follower set is DERIVED here by listing rather than mirrored as a list on
// the canonical, for the reason the collapse itself chose that shape: a
// mirrored list has two writers and desyncs, and a desynced list HERE means a
// silently-skipped follower -- exactly the failure this function exists to
// prevent. Listing is namespace-scoped and filtered, mirroring
// mapSecretToRequests and the budget check.
//
// A List failure drops the re-enqueue (a map func cannot return an error), but
// it is logged and it is not lost work: every Collapsed follower requeues
// itself on collapsedRecheckInterval, so propagation still converges, just
// later.
func (r *Reconciler) mapCanonicalToFollowers(ctx context.Context, o client.Object) []reconcile.Request {
	canonical, ok := o.(*spiceboxv1alpha1.CredentialUpdateRequest)
	if !ok {
		return nil
	}
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := r.Client.List(ctx, &list, client.InNamespace(canonical.Namespace)); err != nil {
		log.FromContext(ctx).Info("credentialupdaterequest: list CredentialUpdateRequests for the follower watch "+
			"failed; dropping re-enqueue (followers still converge on their own recheck interval)",
			"credupdate", canonical.Namespace+"/"+canonical.Name, "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		item := &list.Items[i]
		if item.Name == canonical.Name {
			continue // For() already enqueues the changed object itself
		}
		// Still-Collapsed only. A follower that already settled keeps its
		// status.collapsedInto as the durable record of whose answer it took, so
		// matching on that pointer alone would re-enqueue every settled follower
		// on every canonical write, for a reconcile that can only no-op.
		if item.Status.Phase != spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed {
			continue
		}
		if item.Status.CollapsedInto == nil ||
			item.Status.CollapsedInto.Namespace != canonical.Namespace ||
			item.Status.CollapsedInto.Name != canonical.Name {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name}})
	}
	return out
}

// mapSecretToRequests re-enqueues every Open CredentialUpdateRequest whose
// recorded CredentialSecretRef names the changed Secret -- the "unpark: watch
// the backing Secret" half of this task. A List (not a Get by a
// deterministically-derived name) because a CredentialUpdateRequest's name is
// caller-chosen (the meta tool that creates it), not derived from the
// credential, so there is no name to compute from the Secret alone.
//
// # Why the List is CLUSTER-WIDE
//
// A request does NOT live in its credential's namespace, and for the design's
// headline OAuth case it provably never does. credresolve.SourceFor anchors a
// user-owned type=oauth credential in the shared identities namespace -- that is
// where JIT refresh and the master token live -- while the request lives in its
// session's namespace, and no CredentialUpdateRequest is ever created in the
// identities namespace at all. Restricting the search to the SECRET's namespace
// therefore searched a namespace with no candidates in it and selected nothing,
// for every OAuth card the feature exists to raise: a human pasted the
// replacement, nothing woke the request, and the agent was told at its own
// give-up deadline that nobody had answered.
//
// Correctness does not rest on the List's scope: the per-item comparison below
// carries BOTH halves of the reference (namespace AND name), so a same-named
// Secret in another namespace still selects nothing. The scope only decides
// which requests are CONSIDERED. Cost is bounded the same way the budget List
// is -- requests carry an owner-ref to their session and are GC'd with it, so
// the cluster-wide population is on the order of live sessions -- and the read
// is cache-backed. RBAC already covers it: this controller's
// credentialupdaterequests get;list;watch marker generates a ClusterRole, and
// the Secret watch feeding this function is itself cluster-wide.
func (r *Reconciler) mapSecretToRequests(ctx context.Context, o client.Object) []reconcile.Request {
	sec, ok := o.(*corev1.Secret)
	if !ok {
		return nil
	}
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := r.Client.List(ctx, &list); err != nil {
		log.FromContext(ctx).Info("credentialupdaterequest: list CredentialUpdateRequests for Secret watch failed; dropping re-enqueue (self-heals on next resync)",
			"secret", sec.Namespace+"/"+sec.Name, "err", err.Error())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		item := &list.Items[i]
		if item.Status.Phase != spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen {
			continue
		}
		if item.Status.CredentialSecretRef == nil ||
			item.Status.CredentialSecretRef.Namespace != sec.Namespace ||
			item.Status.CredentialSecretRef.Name != sec.Name {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name}})
	}
	return out
}
