// pkg/channels/channelsd/pipeline/credential_update.go
//
// CredentialUpdateWatcher bridges an operator-decided CredentialUpdateRequest
// (phase=Open) to the user-visible card that lets a human replace the dead
// credential. It mirrors CredentialRequestWatcher's shape (credential_request.go)
// but handles a SINGLE already-resolved credential from the CR's own status,
// not a SessionUserIdentity's whole missing-credentials list.
//
// # Why this lives here, not in the operator
//
// The credentialupdaterequest reconciler DETERMINES ONLY: it resolves identity,
// refreshes, probes, runs Determine, and writes its own CR status. It never
// mints a link, builds a card, or publishes — that is this file's job.
//
// Minting the link here rather than operator-side buys two things:
//   - channelsd holds the passthrough-link HMAC key by NON-OPTIONAL VOLUME
//     MOUNT (pkg/platform/manifests/channelsd/deployment.yaml, read by
//     loadPassthroughSigner), so the kubelet BLOCKS this pod's start when the
//     Secret is missing. An operator reading it live via the API has no such
//     gate and would degrade silently forever. This held once with
//     optional:true: the pod started, the signer stayed nil, and this watcher
//     self-disabled permanently. If you make the mount optional again, this
//     rationale becomes false — delete it rather than leave it lying.
//   - The same HMAC key also produces identityd's idd_session COOKIE payload
//     (iss=identityd), which identityd and webd accept as proof of an
//     authenticated subject. channelsd already holds that capability for
//     credential_link and portal_access; the operator holding it too would be
//     a second, unnecessary custodian of a cookie-forging capability.
//
// # Idempotency: publish at MOST once, record at LEAST once
//
// Showing a card and recording that a card was shown have OPPOSITE retry
// semantics, so ReconcileOne keeps them separable:
//
//   - PUBLISH is at-most-once, behind two guards. Status.InteractionRef != ""
//     is the persisted, cross-restart one. An in-memory UID->requestRef map
//     covers the window before that write is observed: a publish can succeed,
//     its status write conflict, and the next tick re-publish against its own
//     still-empty read. The map entry is written BEFORE the status write for
//     exactly that reason.
//   - RECORD is at-least-once. A pass finding a dedup entry whose ref is still
//     unpersisted re-drives the status write alone rather than skipping the CR.
//     Skipping leaves a durably-failing write retried by nothing, so a card
//     that WAS shown stays recorded as never-shown and both the operator and
//     the tool tell a human that nobody was ever asked.
//
// The map is evicted on a successful record (the persisted ref supersedes it)
// and, via forgetUnseen, for any tracked UID that stops appearing in the List
// at all. It is deliberately NOT evicted while the record keeps failing — that
// entry staying put is what both suppresses the re-publish and drives the
// retry — so its size is bounded by "CRs that exist and have not yet persisted
// InteractionRef", not by process lifetime.
//
// # Delivery is observable, and never claimed falsely
//
// Determination (the operator) and publication (this file, a SEPARATE PROCESS)
// are split, and publication has its own silent skip paths: no InputChannel, no
// started-by subject, no ResolvedCredential, or the whole watcher disabled by a
// malformed signing key. Without a "was anyone actually shown a card" signal, an
// undelivered request still expires operator-side claiming "nobody updated the
// credential before the wait window elapsed" — false; nobody was ever asked.
// recordDelivery stamps CredentialUpdateRequestConditionCardDelivered alongside
// InteractionRef in the same patch, so the operator's expiry Reason and the
// request_credential_update tool's timeout message can tell the cases apart.
//
// The signal is only as honest as its worst case, so the RECORD retry above is
// part of THIS guarantee: a delivered card that never got recorded makes both
// messages claim the exact opposite of the truth.
//
// # Two routes, because a credential has different humans behind it
//
// ReconcileOne splits on WHOSE credential died (ResolvedCredentialRef.AgentOwned):
//
//   - A UserIdentity/SessionUserIdentity credential belongs to a person — the
//     session's starter. The card DMs them.
//   - An AgentIdentity credential is the agent's own SHARED secret. Replacing
//     it requires `agentidentity#update_credential`, which an ordinary starter
//     typically lacks, so the card goes to every role=monitoring Channel and,
//     when the turn's author personally passes that check, into the thread too.
//     See publishAgentOwnedCard.
//
// Routing an agent-owned credential to the starter does not merely show the
// wrong person a card: identityd's PutToken writes their pasted value into
// their OWN UserIdentity, never the AgentIdentity's shared Secret, so the real
// credential stays broken while they walk away believing they fixed it.
package pipeline

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credupdate"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// CredentialUpdateWatcherInterval matches CredentialRequestWatcherInterval --
// the same 5s poll cadence channelsd already uses for the sibling watcher.
const CredentialUpdateWatcherInterval = 5 * time.Second

// DefaultCredentialUpdateLinkTimeout is the fallback link ExpiresAt window when
// LinkTimeout is unset. An ALIAS of credupdate.DefaultLinkLifetime rather than a
// copied value: it is the upper term of the cross-binary ordering
// "tool wait <= park TTL <= link lifetime" that credupdate/timing.go documents.
const DefaultCredentialUpdateLinkTimeout = credupdate.DefaultLinkLifetime

// This site's half of that ordering contract. Converting a negative constant to
// uint64 is a compile error, so this statically asserts the link outlives the
// ask window: shortening it below the operator's park TTL would hand a human who
// clicks late in the window an already-dead link — caught at BUILD time.
const _ = uint64(DefaultCredentialUpdateLinkTimeout - credupdate.DefaultAskWindow) // park TTL <= link lifetime

// CredentialUpdateWatcher polls Open CredentialUpdateRequests with no
// InteractionRef yet, mints a signed deep-link, builds the card, and
// publishes a KindInteractionRequest envelope on the owning session's .out
// subject.
type CredentialUpdateWatcher struct {
	// K8s lists/gets CredentialUpdateRequests + AgentSessions and patches
	// InteractionRef.
	K8s client.Client

	// LinkSigner mints the signed deep-link the card's button points at. Nil
	// disables publishing: ReconcileOne returns an error (logged, retried next
	// tick) rather than emitting a broken/hostless link.
	LinkSigner *passthroughlink.Signer
	// ExternalBaseURL returns the identityd/webd-reachable base URL the link
	// embeds. A getter rather than a string so a live ConfigMap update is
	// picked up without a channelsd restart.
	ExternalBaseURL func() string
	// LinkTimeout bounds the minted link's validity window. Zero defaults to
	// DefaultCredentialUpdateLinkTimeout.
	LinkTimeout time.Duration
	// Now is the time source for the minted link's expiry. Nil defaults to
	// time.Now().UTC(); tests inject a fixed value.
	Now func() time.Time
	// NATSPublish carries the interaction_request envelope, the agent-owned
	// credential's MonitoringEvent, and the best-effort
	// external-URL-unconfigured / no-recipient notices. Required: Run refuses
	// to start when nil.
	NATSPublish channelevents.PublishFunc
	// Authz answers the ONE permission question this watcher asks: may the
	// turn's author replace the agent's own shared credential? Nil is
	// FAIL-CLOSED, not fail-open -- the in-thread admin card is suppressed
	// entirely and agent-owned credentials rely on the monitoring channel
	// alone (logged, per turnAuthorMayUpdateCredential). Declared as the
	// interface type so the zero value is a genuine nil interface; see
	// AGENTS.md "Nil interfaces: never assign a typed-nil pointer".
	Authz CredentialUpdateAuthz

	mu sync.Mutex
	// published records, per CredentialUpdateRequest UID, the requestRef of
	// the card already sent for it -- see the package doc's Idempotency
	// section for exactly which race this closes and why it self-evicts.
	published map[types.UID]string
}

// Run polls until ctx is canceled. Mirrors CredentialRequestWatcher.Run.
func (w *CredentialUpdateWatcher) Run(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("credentialupdate-watcher")
	if w.LinkSigner == nil || w.ExternalBaseURL == nil || w.NATSPublish == nil {
		logger.Info("watcher disabled: missing LinkSigner, ExternalBaseURL, or NATSPublish")
		return
	}
	ticker := time.NewTicker(CredentialUpdateWatcherInterval)
	defer ticker.Stop()

	w.reconcileAll(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcileAll(ctx, logger)
		}
	}
}

// reconcileAll lists every CredentialUpdateRequest cluster-wide and dispatches
// the ones awaiting a card. Cluster-wide because a session — and so its
// requests — can live in any namespace, leaving no single namespace to scope to.
// Per-request errors are logged, not returned; the next tick retries.
//
// Cost note: this is NOT bounded by live asks. A CredentialUpdateRequest is
// owner-ref'd to its AgentSession and lives as long as the session does — the
// operator's budgetExceeded depends on that, counting every ask a session has
// SPENT. So every request ever created, for every running session in the
// cluster, is in this List every 5s: growth is CUMULATIVE, not concurrent. Fine
// at expected scale; a field-indexed List on phase=Open would be cheaper.
func (w *CredentialUpdateWatcher) reconcileAll(ctx context.Context, logger logr.Logger) {
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	if err := w.K8s.List(ctx, &list); err != nil {
		logger.Error(err, "list CredentialUpdateRequests")
		return
	}
	seen := make(map[types.UID]struct{}, len(list.Items))
	for i := range list.Items {
		cur := &list.Items[i]
		// Marked seen regardless of phase: the CR still EXISTS, and only a
		// genuinely deleted one — absent from every future List — may be pruned.
		seen[cur.UID] = struct{}{}
		// Every listed CR goes to ReconcileOne, which owns the whole gate
		// (phase, persisted ref, dedup entry) in ONE place. Pre-filtering on
		// phase==Open here would duplicate that decision AND silently exclude
		// the case most needing a retry: a CR whose card WAS published, whose
		// delivery record never landed, and which has since gone terminal.
		// ReconcileOne returns on a map lookup when there is nothing to do.
		if err := w.ReconcileOne(ctx, cur); err != nil {
			logger.Info("credential_update reconcile failed",
				"credupdate", cur.Namespace+"/"+cur.Name, "err", err.Error())
			continue
		}
	}
	w.forgetUnseen(seen)
}

// ReconcileOne does the per-request work for one CredentialUpdateRequest:
// publish a card for an Open, undelivered one, or -- for one whose card was
// already published but whose delivery record never landed -- retry just that
// record. Exported so tests can drive it directly.
//
// The two are deliberately separate steps with OPPOSITE retry semantics; see
// recordDelivery.
func (w *CredentialUpdateWatcher) ReconcileOne(ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest) error {
	logger := log.FromContext(ctx).WithValues("credupdate", cur.Namespace+"/"+cur.Name)

	if cur.Status.InteractionRef != "" {
		// Delivery is already recorded and persisted. Any dedup entry still
		// held for this UID is dead weight from here on: the persisted ref
		// alone suppresses a re-publish, from this process and from a
		// restarted one. Evicting here (as well as on the write that set it)
		// covers the case where THIS process is not the one that wrote it.
		w.forgetPublished(cur.UID)
		return nil
	}
	if ref, ok := w.alreadyPublished(cur.UID); ok {
		// A card WAS published, but the status write recording it has not
		// landed — either still in flight (the List predates it) or failed on
		// an earlier pass. Retry ONLY the record; never a second publish.
		//
		// Returning nil here would leave a durably-failing record retried by
		// nothing, since the entry is evicted only on a successful write or the
		// CR's deletion. A delivered card would stay recorded forever as
		// never-delivered, making the operator's expiry Reason and the tool's
		// timeout message both claim nobody was asked — the exact INVERSE of
		// the falsehood CardDelivered exists to prevent, and the more corrosive
		// direction, because it teaches people to distrust a usually-true
		// message.
		return w.recordDelivery(ctx, cur, ref, logger)
	}
	if cur.Status.Phase != spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen {
		// Nothing published for it and no longer awaiting a card.
		return nil
	}
	if cur.Status.ResolvedCredential == nil {
		// The session ref on the spec is the only addressee available here --
		// the session itself has not been read yet, and cannot be: which
		// credential failed is what the rest of this function is built around.
		return w.handleUserOwnedNoRecipient(ctx, cur, cur.Spec.SessionRef.Namespace, cur.Spec.SessionRef.Name,
			"the request is Open but no credential was resolved onto it, so there is nothing to build a card for", logger)
	}

	var sess spiceboxv1alpha1.AgentSession
	sessKey := client.ObjectKey{Namespace: cur.Spec.SessionRef.Namespace, Name: cur.Spec.SessionRef.Name}
	if err := w.K8s.Get(ctx, sessKey, &sess); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("AgentSession not found; will retry next tick", "session", sessKey.String())
			return nil
		}
		return fmt.Errorf("get AgentSession %s: %w", sessKey, err)
	}

	// The agent's OWN shared credential has different humans behind it, so it
	// routes differently. Branched BEFORE the InputChannel/starter gates
	// deliberately: those are the user-owned path's preconditions — its
	// recipient IS the starter, reached through the session's own channel —
	// and neither is needed to reach a platform admin on monitoring.
	if cur.Status.ResolvedCredential.AgentOwned() {
		return w.publishAgentOwnedCard(ctx, cur, &sess, logger)
	}

	// The user-owned route's preconditions for having anyone to deliver to: a
	// channel to render on, and an addressee to render for. Neither should
	// normally be missing, since identityMode=userPassthrough already requires
	// a channel-attached session with a known starter — but skipping silently
	// leaves the agent burning its whole ask timeout before reporting that
	// nobody answered a card nobody was sent.
	if sess.Spec.InputChannel == nil {
		return w.handleUserOwnedNoRecipient(ctx, cur, sess.Namespace, sess.Name,
			"the session is not attached to a channel, so there is no surface to deliver the card on", logger)
	}

	starter := spiceboxv1alpha1.StartedBySubject(&sess)
	if starter == "" {
		return w.handleUserOwnedNoRecipient(ctx, cur, sess.Namespace, sess.Name,
			"the session records no started-by identity, so the card has no addressee", logger)
	}

	if w.LinkSigner == nil {
		return fmt.Errorf("credential_update: LinkSigner not configured (wiring bug)")
	}
	externalURL := ""
	if w.ExternalBaseURL != nil {
		externalURL = w.ExternalBaseURL()
	}
	if externalURL == "" {
		return w.handleExternalURLUnconfigured(ctx, cur, &sess, logger)
	}

	linkURL, err := w.mintCardLink(cur, &sess, externalURL, starter)
	if err != nil {
		return err
	}

	// The requester's channel-scoped identity. Kind MUST be the session's own
	// channel kind: the Slack sender's `if r.Kind != "slack"` skips a recipient
	// without it, silently dropping the card. ExternalID+Email carry the natural
	// raw+email form; Subject is the fallback only when no verified email is on
	// record — the same construction doPublish uses for its starterIdentity.
	recipient := channelevents.ExternalIdentity{
		Kind:       identity.Kind(sess.Spec.InputChannel.Kind),
		ExternalID: spiceboxv1alpha1.StartedByExternalID(&sess),
		Email:      spiceboxv1alpha1.StartedByEmail(&sess),
	}
	if recipient.Email == "" {
		recipient.Subject = starter
	}

	payload := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.CredentialUpdate,
		RequestRef:      mintRequestID(),
		// Lead is the verdict line, sanitized the SAME way Body's attributed why
		// is. It only LOOKS platform-authored: for VerifyRejected, Reason IS the
		// provider's raw HTTP response body (ProbeDetail, up to 200 bytes), so
		// it can carry newlines, markup or control characters a hostile or
		// merely buggy provider chose. A no-op for a clean platform sentence.
		Lead:     sanitizeCardText(cur.Status.Reason),
		Body:     attributedWhy(cur.Spec.Why),
		Fields:   cardFields(cur),
		Actions:  cardActions(linkURL),
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &recipient},
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("credential_update: built an invalid interaction_request payload: %w", err)
	}
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name, channelevents.KindInteractionRequest, payload); err != nil {
		return fmt.Errorf("credential_update: publish interaction_request (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	// Recorded BEFORE the status write, deliberately: from this instant a
	// human may already be looking at the card, so no later pass may publish
	// a second one -- whatever happens to the write below.
	w.recordPublished(cur.UID, payload.RequestRef)

	if err := w.recordDelivery(ctx, cur, payload.RequestRef, logger); err != nil {
		return err
	}

	logger.Info("credential_update published", "recipient", starter, "requestRef", payload.RequestRef)
	return nil
}

// CredentialUpdateAuthz is the ONE permission question this watcher asks:
// may this human replace the agent's own shared credential?
//
// Kept as its own single-method interface rather than a method added to
// pipeline.Authz on purpose: Authz is implemented by fakes that live in
// build-tagged integration/e2e test files `go test ./...` never compiles
// (AGENTS.md's ship-gate note), so widening it to serve one watcher would
// break suites the default test run cannot see. Nothing else needs this
// question, so nothing else should have to grow a method to answer it.
//
// Implemented by *spicedb.Client. Consistency is NOT a parameter -- see
// spicedb.Client.CheckAgentIdentityUpdateCredential for why a caller must not
// be able to ask this question at MinimizeLatency.
type CredentialUpdateAuthz interface {
	CheckAgentIdentityUpdateCredential(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID) (bool, error)
}

// publishAgentOwnedCard delivers the card for an agent's OWN shared credential
// to the humans who may actually replace it, on up to two surfaces:
//
//	a. Every role=monitoring Channel, via a MonitoringEvent. This is the
//	   standing surface: `agentidentity#update_credential` resolves through
//	   platform->can_admin, and platform admins are not generally in the
//	   agent's thread. It is also the ONLY surface reachable from here — the
//	   outbound relay routes an interaction_request to the SESSION's own
//	   InputChannel, so no envelope can address an arbitrary Channel; only the
//	   monitoring-event fan-out can.
//	b. The session thread itself, as a normal card, IFF the turn's author
//	   personally passes the check. That click is subject-bound (the link
//	   carries their subject) and needs no separate sign-in hunt, so when the
//	   person who asked is also allowed to fix it, they fix it in place.
//
// Both surfaces are attempted; either one landing counts as delivery, and the
// existing at-most-once dedup + at-least-once record machinery then applies
// unchanged (one requestRef, one recordDelivery, the same in-memory UID map).
// A partial failure is logged and NOT retried: a second pass would re-publish
// the surface that already worked, and a duplicate card is worse than a
// single-surface one. The case where NEITHER surface lands is the one this
// route exists to make loud -- see handleNoRecipient.
func (w *CredentialUpdateWatcher) publishAgentOwnedCard(
	ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest,
	sess *spiceboxv1alpha1.AgentSession, logger logr.Logger,
) error {
	if w.LinkSigner == nil {
		return fmt.Errorf("credential_update: LinkSigner not configured (wiring bug)")
	}
	externalURL := ""
	if w.ExternalBaseURL != nil {
		externalURL = w.ExternalBaseURL()
	}
	if externalURL == "" {
		return w.handleExternalURLUnconfigured(ctx, cur, sess, logger)
	}

	rc := cur.Status.ResolvedCredential
	logger = logger.WithValues("identity", rc.Namespace+"/"+rc.Name, "credential", rc.Credential)

	requestRef := mintRequestID()
	delivered := 0
	// unreached accumulates, per surface, WHY it did not deliver -- so the
	// no-recipient surfacing below can state what actually happened instead of
	// inferring it from a single boolean. Every entry is written at the point
	// the fact is known; none is reconstructed after the fact.
	var unreached []string

	// Cost note: this resolves every role=monitoring Channel (a List plus a
	// Secret Get per candidate) on every 5s tick for as long as an agent-owned
	// request stays parked and undelivered. Monitoring Channels are few (the
	// relay resolves them fresh per event for the same reason), and this runs
	// only for parked AgentIdentity requests, so it is bounded in practice --
	// but a cluster that parks many of them at once would want this cached.
	monitored, whyNot, err := monitoringRecipientExists(ctx, w.K8s)
	if err != nil {
		// Cannot tell whether a recipient exists -> cannot honestly claim
		// delivery OR no-recipient. Retry next tick.
		return fmt.Errorf("credential_update: resolve a monitoring recipient: %w", err)
	}
	switch {
	case !monitored:
		// Logged even when the in-thread card below succeeds: a cluster with
		// no deliverable monitoring Channel can only ever reach an admin by
		// the accident of one happening to be in the thread, and the operator
		// should be able to grep for that.
		logger.Info("credential_update: no deliverable role=monitoring Channel, so the standing admin surface is unreachable "+
			"for this agent-owned credential; a platform admin will not see this unless they are in the thread", "detail", whyNot)
		unreached = append(unreached, whyNot)
	default:
		if err := w.publishAgentOwnedMonitoring(cur, sess, externalURL, logger); err != nil {
			logger.Info("credential_update: monitoring-channel delivery failed", "err", err.Error())
			unreached = append(unreached, "the monitoring-channel broadcast failed: "+err.Error())
		} else {
			delivered++
		}
	}

	if mayUpdate, whyNotAuthor := w.turnAuthorMayUpdateCredential(ctx, cur, logger); !mayUpdate {
		unreached = append(unreached, whyNotAuthor)
	} else if err := w.publishAgentOwnedInThread(cur, sess, externalURL, requestRef, logger); err != nil {
		logger.Info("credential_update: in-thread admin card delivery failed", "err", err.Error())
		unreached = append(unreached, "the in-thread admin card failed to publish: "+err.Error())
	} else {
		delivered++
	}

	if delivered == 0 {
		return w.handleNoRecipient(ctx, cur, sess, unreached, logger)
	}

	// Recorded BEFORE the status write, deliberately: from this instant a
	// human may already be looking at the card, so no later pass may publish
	// a second one -- whatever happens to the write below.
	w.recordPublished(cur.UID, requestRef)
	if err := w.recordDelivery(ctx, cur, requestRef, logger); err != nil {
		return err
	}
	logger.Info("credential_update published for an agent-owned credential",
		"monitoringChannel", monitored, "surfaces", delivered, "requestRef", requestRef)
	return nil
}

// publishAgentOwnedMonitoring emits the MonitoringEvent that carries the card
// to every role=monitoring Channel.
//
// The embedded link is deliberately NOT subject-bound: a monitoring channel has
// no single addressee to bind to. Anyone who can read the channel, or anyone the
// URL is forwarded to, therefore holds it, and the ONLY control is at click
// time — identityd proves a subject from the idd_session cookie and runs a live
// agentidentity#update_credential check before rendering the form or accepting a
// value, and the operator re-runs that check authoritatively before writing.
//
// identityd's starter-match step is deliberately BYPASSED for an agent-owned
// link: a subject-less link can never equal a starter, and the permission check
// replacing it is strictly stronger.
func (w *CredentialUpdateWatcher) publishAgentOwnedMonitoring(
	cur *spiceboxv1alpha1.CredentialUpdateRequest, sess *spiceboxv1alpha1.AgentSession,
	externalURL string, logger logr.Logger,
) error {
	linkURL, err := w.mintCardLink(cur, sess, externalURL, "")
	if err != nil {
		return err
	}
	rc := cur.Status.ResolvedCredential

	// Summary is the card body, one fact per line. The channel kind block-quotes
	// it, and every piece is either platform-authored or run through
	// sanitizeCardText — which collapses newlines — so no single piece can grow
	// a second line and pose as one of the others.
	//
	// ToolName is sanitized defensively, not against a live threat: the runner
	// records the tool's LLM-facing name, already reduced by
	// synthesize.NormalizeName to [a-zA-Z0-9_-]{1,128}. The CRD field carries no
	// pattern of its own, though, so nothing structurally holds that invariant,
	// and this is the cheapest place to make a violation harmless. cardFields
	// sanitizes the same value for the same reason. Escaping the rendered result
	// is the kind's job; this only guarantees single-line, control-free input.
	lines := []string{
		sanitizeCardText(cur.Status.Reason),
		blastRadiusLine(rc),
		fmt.Sprintf("Failing tool: %s (session %s/%s).", sanitizeCardText(cur.Spec.ToolName), sess.Namespace, sess.Name),
	}
	if why := attributedWhy(cur.Spec.Why); why != "" {
		lines = append(lines, why)
	}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      spiceboxv1alpha1.IdentityKindAgentIdentity,
			Namespace: rc.Namespace,
			Name:      rc.Name,
		},
		Condition: spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending,
		Reason:    cur.Status.Determination,
		Summary:   strings.Join(lines, "\n"),
		Hint:      updateCredentialActionLabel + ": " + linkURL,
		Timestamp: w.now(),
	}
	if err := channelevents.PublishMonitoring(w.NATSPublish, ev); err != nil {
		return fmt.Errorf("publish credential-update monitoring event: %w", err)
	}
	logger.Info("credential_update: published to the monitoring channel(s)")
	return nil
}

// publishAgentOwnedInThread posts the SAME card into the session thread,
// addressed to the turn's author -- who has already been checked to hold
// agentidentity#update_credential by the caller.
//
// Unlike the monitoring link this one IS subject-bound: there is a specific
// addressee, so binding costs nothing and makes a forwarded link inert.
func (w *CredentialUpdateWatcher) publishAgentOwnedInThread(
	cur *spiceboxv1alpha1.CredentialUpdateRequest, sess *spiceboxv1alpha1.AgentSession,
	externalURL, requestRef string, logger logr.Logger,
) error {
	if sess.Spec.InputChannel == nil {
		// The author passed the permission check but there is no channel to
		// render a card on. Returned as an error so the caller files it under
		// `unreached` -- which is NOT the same as failing the reconcile: the
		// caller counts surfaces, and the monitoring broadcast may well have
		// delivered on its own.
		logger.Info("credential_update: session has no InputChannel, so the in-thread admin card cannot be rendered")
		return fmt.Errorf("session %s/%s has no InputChannel", sess.Namespace, sess.Name)
	}
	author := cur.Spec.RequestedBy
	linkURL, err := w.mintCardLink(cur, sess, externalURL, author)
	if err != nil {
		return err
	}

	// Kind is mandatory: a kind drops every recipient whose Kind is not its own
	// before it ever calls Principal(), so a Kind-less requester makes the whole
	// card vanish silently. The starter's raw+email annotations are reused ONLY
	// when the author IS the starter; for any other author all we hold is their
	// canonical subject, which Principal() treats as authoritative.
	recipient := channelevents.ExternalIdentity{Kind: identity.Kind(sess.Spec.InputChannel.Kind)}
	if starter := spiceboxv1alpha1.StartedBySubject(sess); starter == author {
		recipient.ExternalID = spiceboxv1alpha1.StartedByExternalID(sess)
		recipient.Email = spiceboxv1alpha1.StartedByEmail(sess)
	}
	if recipient.Email == "" && recipient.ExternalID == "" {
		recipient.Subject = author
	}

	payload := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		Category:        categories.CredentialUpdate,
		RequestRef:      requestRef,
		Lead:            sanitizeCardText(cur.Status.Reason),
		Body:            attributedWhy(cur.Spec.Why),
		Fields:          cardFields(cur),
		Actions:         cardActions(linkURL),
		Audience:        channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &recipient},
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("built an invalid interaction_request payload: %w", err)
	}
	if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name, channelevents.KindInteractionRequest, payload); err != nil {
		return fmt.Errorf("publish interaction_request (session %s/%s): %w", sess.Namespace, sess.Name, err)
	}
	logger.Info("credential_update: published the in-thread admin card", "recipient", author.String(), "requestRef", requestRef)
	return nil
}

// turnAuthorMayUpdateCredential reports whether the author of the turn that
// produced this request personally holds agentidentity#update_credential on
// the identity whose credential died, and -- when it does not -- WHY.
//
// FAIL-CLOSED in every branch that is not an outright "yes": no authorization
// client wired, no/!user author subject, or a failed check all mean "do not
// render the in-thread card".
//
// The reason is returned, not merely logged, because the CR condition is the
// DURABLE surface an operator reads long after the log line rotated — and these
// four causes call for four different responses. Collapsing them to "the author
// does not hold the permission" sends an operator whose channelsd lost its
// SpiceDB client off to grant a permission that was never the problem.
func (w *CredentialUpdateWatcher) turnAuthorMayUpdateCredential(
	ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest, logger logr.Logger,
) (bool, string) {
	rc := cur.Status.ResolvedCredential
	if w.Authz == nil {
		logger.Info("credential_update: no authorization client is wired into this watcher, so the in-thread admin card " +
			"is suppressed (fail-closed); every agent-owned credential now depends on a monitoring Channel alone")
		return false, "the in-thread card could not be offered: channelsd has no authorization client wired, so no permission could be checked (a wiring bug, not a missing grant)"
	}
	author := cur.Spec.RequestedBy
	if author.Empty() {
		logger.Info("credential_update: request records no turn author, so no in-thread admin card can be offered")
		return false, "the in-thread card could not be offered: the request records no turn author to address it to"
	}
	canonical, err := author.CanonicalUserID()
	if err != nil {
		logger.Info("credential_update: turn author is not a user subject; no in-thread admin card",
			"author", author.String(), "err", err.Error())
		return false, fmt.Sprintf("the in-thread card could not be offered: turn author %q is not a user subject (%v)", author.String(), err)
	}
	ok, err := w.Authz.CheckAgentIdentityUpdateCredential(ctx, rc.Namespace, rc.Name, canonical)
	if err != nil {
		logger.Info("credential_update: update_credential check failed; suppressing the in-thread admin card (fail-closed)",
			"author", author.String(), "err", err.Error())
		return false, fmt.Sprintf("the in-thread card could not be offered: the update_credential check for %q FAILED and was refused fail-closed (%v) -- this is an authorization-service fault, not a denial", author.String(), err)
	}
	if !ok {
		return false, fmt.Sprintf("this turn's author (%s) does not hold agentidentity#update_credential", author.String())
	}
	return true, ""
}

// handleNoRecipient is the fail-closed path when an agent-owned credential's
// card reached NOBODY: no monitoring Channel to broadcast to (or the broadcast
// failed) and no in-thread admin either.
//
// Loud for the same reason handleExternalURLUnconfigured is: a request parked
// with no reachable recipient is otherwise indistinguishable from one a human is
// still thinking about. It would sit Open for its whole window, expire, and tell
// the agent nobody acted — when nobody was ever asked. That is the silent-hang
// class this project has shipped more than once.
//
// surfaceNoRecipient does the surfacing, shared with the user-owned route; this
// contributes only the copy naming WHAT was unreachable and why.
func (w *CredentialUpdateWatcher) handleNoRecipient(
	ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest,
	sess *spiceboxv1alpha1.AgentSession, unreached []string, logger logr.Logger,
) error {
	rc := cur.Status.ResolvedCredential
	detail := strings.Join(unreached, "; ")
	return w.surfaceNoRecipient(ctx, cur, sess.Namespace, sess.Name, noRecipientNotice{
		LogMessage: "credential_update: no reachable recipient for an agent-owned credential",
		LogDetail:  detail,
		CondMessage: fmt.Sprintf("%s's %q credential is the agent's own shared credential, so only a platform administrator "+
			"(agentidentity#update_credential) may replace it -- but nobody could be shown the card: %s",
			rc.Namespace+"/"+rc.Name, rc.Credential, detail),
		// Deliberately does NOT repeat the platform verdict line: unlike the
		// card, this text goes to whoever is in the thread rather than to an
		// authorized admin, and Status.Reason can carry up to 200 bytes of the
		// provider's raw response.
		UserText: fmt.Sprintf("This needs a platform administrator: %s uses %s's %q credential, which the platform "+
			"verified is no longer working. Replacing a shared agent credential isn't something I can ask you to do -- "+
			"please ask your operator to configure a monitoring channel (or to grant an admin here the permission), "+
			"and this will resolve itself.", cur.Spec.ToolName, rc.Namespace+"/"+rc.Name, rc.Credential),
		UserShort: "Needs a platform administrator…",
	}, logger)
}

// handleUserOwnedNoRecipient is the user-owned route's counterpart to
// handleNoRecipient: the same three surfaces, for the three ways this route
// can find itself with nowhere to send the card (no resolved credential, no
// channel to render on, no started-by identity to address).
//
// All three are loud — condition, notification, and retry — because logging at
// Info and returning nil is the silent-hang shape: the agent burns its whole ask
// timeout before anyone learns nobody was asked.
//
// detail is operator-facing and lands on the CR condition. The thread-facing
// text is the SAME sentence for all three: the person waiting cannot act on any
// of these causes, and which internal precondition is missing is not theirs.
//
// sessNS/sessName come from the caller rather than a *AgentSession because the
// earliest of the three fires before the session has been read; they are the
// request's own SessionRef, which is what addresses the notification anyway.
func (w *CredentialUpdateWatcher) handleUserOwnedNoRecipient(
	ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest,
	sessNS, sessName, detail string, logger logr.Logger,
) error {
	return w.surfaceNoRecipient(ctx, cur, sessNS, sessName, noRecipientNotice{
		LogMessage:  "credential_update: no reachable recipient for a user-owned credential",
		LogDetail:   detail,
		CondMessage: "the credential-update card could not be shown to anybody: " + detail,
		UserText: "I need a credential updated before I can carry on, but I couldn't show you the form to do it. " +
			"Please ask your operator to take a look -- this will resolve itself once it's fixed.",
		UserShort: "Couldn't show the credential form…",
	}, logger)
}

// noRecipientNotice describes ONE no-recipient failure across surfaceNoRecipient's
// three surfaces. Split from the surfacing so each route states its own cause
// while both share one condition key, one dedup rule, and one notification shape.
type noRecipientNotice struct {
	// LogMessage + LogDetail are operator-facing, logged on the first
	// transition only.
	LogMessage string
	LogDetail  string
	// CondMessage lands on CredentialUpdateRequestConditionCardDelivered=False
	// and is read by an operator, so it names the precise cause.
	CondMessage string
	// UserText + UserShort go to whoever is in the session thread. They must
	// read as an explanation of what the agent is blocked on, never as an
	// operator runbook -- no resource names, no field paths.
	UserText  string
	UserShort string
}

// surfaceNoRecipient is the ONE place either route reports "the card reached
// nobody". It writes all three surfaces:
//
//	a. CredentialUpdateRequestConditionCardDelivered=False/NoRecipient on the
//	   CR, which the operator's expiry branch reads to say "this was never
//	   delivered" rather than "nobody updated it in time".
//	b. A session-visible KindNotification, so the humans in the thread learn
//	   the agent is blocked rather than watching it stall.
//	c. A returned error, so reconcileAll logs it and the next tick retries --
//	   which is what makes this recover on its own once the missing piece
//	   appears.
//
// InteractionRef stays EMPTY, deliberately: stamping a ref for a card nobody
// saw would make the operator's honest "never delivered" message lie.
//
// (a) and (b) fire only on the FIRST transition into this state: reconcileAll
// re-dispatches the same CR every 5s for the whole park, so an unconditional
// notification would be hundreds of writes per session. The condition is the
// dedup key, re-read fresh from cur each call, and is distinguished from
// handleExternalURLUnconfigured's by Reason — so a request flipping between the
// two failures still re-surfaces when the cause genuinely changes.
func (w *CredentialUpdateWatcher) surfaceNoRecipient(
	ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest,
	sessNS, sessName string, n noRecipientNotice, logger logr.Logger,
) error {
	const reason = spiceboxv1alpha1.ReasonCredentialUpdateNoRecipient

	prev := conditions.Find(cur.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	firstTransition := prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason

	if firstTransition {
		logger.Info(n.LogMessage, "detail", n.LogDetail)
		if err := channelevents.PublishOut(w.NATSPublish, sessNS, sessName,
			channelevents.KindNotification,
			channelevents.NotificationPayload{Text: n.UserText, Short: n.UserShort},
		); err != nil {
			logger.Info("credential_update: publish no-recipient notification failed (best-effort, continuing)",
				"err", err.Error())
		}
		prior := cur.DeepCopy()
		conditions.SetFalse(cur, &cur.Status.Conditions,
			spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered, reason, n.CondMessage)
		if err := w.K8s.Status().Patch(ctx, cur, client.MergeFrom(prior)); err != nil {
			logger.Info("credential_update: patch CardDelivered=False/NoRecipient failed; will retry next tick",
				"err", err.Error())
		}
	}
	return fmt.Errorf("credential_update: %s", n.CondMessage)
}

// mintCardLink mints the signed deep-link the card's single action points at.
//
// subject binds the link to one person: identityd refuses it for anyone else.
// Pass "" ONLY for a surface with no single addressee (the monitoring
// broadcast), where the click has to be authorized at identityd instead.
func (w *CredentialUpdateWatcher) mintCardLink(
	cur *spiceboxv1alpha1.CredentialUpdateRequest, sess *spiceboxv1alpha1.AgentSession,
	externalURL string, subject identity.Subject,
) (string, error) {
	timeout := w.LinkTimeout
	if timeout <= 0 {
		timeout = DefaultCredentialUpdateLinkTimeout
	}
	raw, err := w.LinkSigner.Mint(passthroughlink.Payload{
		SessionRef:          sess.Namespace + "/" + sess.Name,
		Subject:             subject,
		RequiredCredentials: []string{cur.Status.ResolvedCredential.Credential},
		Purpose:             passthroughlink.PurposeCredentialUpdate,
		ExpiresAt:           w.now().Add(timeout).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("mint credential-update link: %w", err)
	}
	linkURL, err := buildLinkURL(externalURL, raw)
	if err != nil {
		return "", fmt.Errorf("build credential-update link: %w", err)
	}
	return linkURL, nil
}

// updateCredentialActionLabel is the platform-authored label of the card's
// ONE action, written here once so the interaction button and the monitoring
// event's hint can never describe the same click differently.
const updateCredentialActionLabel = "Update credential"

// cardActions returns the card's action list: exactly one, platform-labelled.
func cardActions(linkURL string) []channelevents.InteractionAction {
	return []channelevents.InteractionAction{{
		ID:    "update_credential",
		Label: updateCredentialActionLabel,
		Style: channelevents.ActionStylePrimary,
		Kind:  channelevents.ActionKindLink,
		URL:   linkURL,
	}}
}

// cardFields returns the card's label/value rows. The identity and credential
// values are platform-authored (names off the CR's own resolved status); the
// agent's only free-text contribution to this card is Body's attributed why.
//
// ToolName is the one row that is neither: it is written onto the CR by the
// meta tool from the failing tool's LLM-facing name, and the field carries no
// schema constraint of its own, so it is run through sanitizeCardText for the
// same reason publishAgentOwnedMonitoring does — the two render the same value
// and must not disagree about whether it is trusted.
//
// For an agent-owned credential a blast-radius row is appended: the identity
// and credential names alone do not tell an admin that the thing they are
// about to replace is SHARED. Without it, the card reads exactly like the
// user-owned one, and an admin can reasonably believe they are re-linking
// their own account rather than rotating a token every session running that
// agent authenticates with.
func cardFields(cur *spiceboxv1alpha1.CredentialUpdateRequest) []channelevents.InteractionField {
	rc := cur.Status.ResolvedCredential
	fields := []channelevents.InteractionField{
		{Label: "Identity", Value: fmt.Sprintf("%s %s", rc.IdentityKind, rc.Name)},
		{Label: "Credential", Value: rc.Credential},
		{Label: "Tool", Value: sanitizeCardText(cur.Spec.ToolName)},
	}
	if rc.AgentOwned() {
		fields = append(fields, channelevents.InteractionField{Label: "Affects", Value: blastRadiusLine(rc)})
	}
	return fields
}

// blastRadiusLine is the single platform-authored sentence naming what a
// replacement actually changes. Shared by the card field and the monitoring
// event so the two can never disagree about the stakes.
func blastRadiusLine(rc *spiceboxv1alpha1.ResolvedCredentialRef) string {
	return fmt.Sprintf("%q is %s's OWN shared credential, not a personal one: replacing it changes what EVERY session running this agent authenticates as.",
		rc.Credential, rc.Namespace+"/"+rc.Name)
}

// recordDelivery persists that a card WAS shown, stamping Status.InteractionRef
// and CredentialUpdateRequestConditionCardDelivered=True in ONE Status().Patch
// so the two can never disagree. The operator's Expired-transition Reason and
// the request_credential_update tool's timeout message both branch on that pair
// to tell "nobody updated it in time" from "nobody was ever asked" — a
// distinction neither can observe any other way, since publication happens in a
// separate process with its own silent skip paths.
//
// Kept separate from the publish because the two have OPPOSITE retry semantics
// and must not share a return; see the package doc's Idempotency section.
//
// On success the dedup entry is evicted: the persisted ref takes over as the
// durable cross-restart re-publish guard, so keeping the in-memory one would
// only grow the map for the process's lifetime.
//
// CAVEAT on the agent-owned route: requestRef is minted per REQUEST, but only
// the in-thread card is an interaction that carries it on the wire -- the
// monitoring broadcast is a MonitoringEvent, which has no requestRef field.
// When the broadcast is the only surface that landed, InteractionRef therefore
// records a ref no delivered artifact carries. That is inert today: the
// condition and the ref are read only as "was anything delivered", and the
// monitoring surface's action is an out-of-band link that resolves through the
// Secret watch rather than through an interaction_decision. It would stop
// being inert the moment something tries to correlate a decision back to the
// broadcast; at that point the ref belongs IN the event.
func (w *CredentialUpdateWatcher) recordDelivery(ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest,
	requestRef string, logger logr.Logger) error {
	prior := cur.DeepCopy()
	cur.Status.InteractionRef = requestRef
	conditions.SetTrue(cur, &cur.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered,
		spiceboxv1alpha1.ReasonCredentialUpdateCardDelivered)
	if err := w.K8s.Status().Patch(ctx, cur, client.MergeFrom(prior)); err != nil {
		logger.Info("credential_update: recording delivery failed; the card WAS published, so no second card will be sent -- a later pass retries this write alone",
			"requestRef", requestRef, "err", err.Error())
		return fmt.Errorf("patch InteractionRef: %w", err)
	}
	w.forgetPublished(cur.UID)
	return nil
}

// handleExternalURLUnconfigured is the fail-closed path when
// w.ExternalBaseURL() returns "". It best-effort notifies the session so the
// waiting user is not left in silence, and returns an error so reconcileAll
// logs it and retries next tick — recovering on its own once the operator
// configures the external-URL ConfigMap. Deliberately narrower than
// CredentialRequestWatcher's three-surface version: no monitoring broadcast.
//
// The notification fires ONLY on the first transition into this state. Without
// a dedup, reconcileAll re-dispatches this same CR every 5s for the entire park
// (it stays Open with InteractionRef==""), publishing a fresh KindNotification
// every tick — hundreds of status-caption writes per park.
// CredentialUpdateRequestConditionCardDelivered=False, with a reason unique to
// this failure, is the dedup key, re-read fresh from cur each call.
func (w *CredentialUpdateWatcher) handleExternalURLUnconfigured(
	ctx context.Context, cur *spiceboxv1alpha1.CredentialUpdateRequest, sess *spiceboxv1alpha1.AgentSession, logger logr.Logger,
) error {
	const reason = "ExternalURLUnconfigured"
	prev := conditions.Find(cur.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	firstTransition := prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason

	if firstTransition {
		text := "Updating this credential is temporarily unavailable: the platform's web address isn't configured. " +
			"Please ask your operator to set it; this will resolve automatically once it is."
		if err := channelevents.PublishOut(w.NATSPublish, sess.Namespace, sess.Name,
			channelevents.KindNotification,
			channelevents.NotificationPayload{Text: text, Short: "Credential update unavailable…"},
		); err != nil {
			logger.Info("credential_update: publish external-URL-unconfigured notification failed (best-effort, continuing)",
				"err", err.Error())
		}
		prior := cur.DeepCopy()
		conditions.SetFalse(cur, &cur.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered,
			reason, fmt.Sprintf("the platform's external web address is not configured (%s ConfigMap); the credential-update card cannot be built",
				spiceboxv1alpha1.WebdExternalURLConfigMap))
		if err := w.K8s.Status().Patch(ctx, cur, client.MergeFrom(prior)); err != nil {
			logger.Info("credential_update: patch CardDelivered=False failed; will retry next tick", "err", err.Error())
		}
	}
	return fmt.Errorf("webd external URL not configured (%s ConfigMap); cannot build credential-update link for session %s/%s",
		spiceboxv1alpha1.WebdExternalURLConfigMap, sess.Namespace, sess.Name)
}

func (w *CredentialUpdateWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

func (w *CredentialUpdateWatcher) alreadyPublished(uid types.UID) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ref, ok := w.published[uid]
	return ref, ok
}

func (w *CredentialUpdateWatcher) recordPublished(uid types.UID, ref string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.published == nil {
		w.published = make(map[types.UID]string)
	}
	w.published[uid] = ref
}

func (w *CredentialUpdateWatcher) forgetPublished(uid types.UID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.published, uid)
}

// forgetUnseen prunes every tracked UID absent from the most recent
// reconcileAll List — the backing CR no longer exists, owner-ref GC having
// reaped it with its AgentSession. This is the ONE eviction safe to add on top
// of forgetPublished's on-success one: a durably-failing InteractionRef patch
// MUST keep its entry (the package doc's Idempotency section says why evicting
// that one reopens the race this map closes), but a CR that stopped existing can
// never be re-published to, so keeping its entry is a leak with no safety gain.
func (w *CredentialUpdateWatcher) forgetUnseen(seen map[types.UID]struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for uid := range w.published {
		if _, ok := seen[uid]; !ok {
			delete(w.published, uid)
		}
	}
}

// attributionPrefix is the ONLY place "The agent said:" is written. Every
// consumer of Body (attributedWhy's own no-dangling-block guarantee, tests
// asserting attribution) references this constant rather than the literal,
// so the two can never drift.
const attributionPrefix = "The agent said: "

// attributedWhy renders the agent's UNTRUSTED why, visibly attributed and
// sanitized -- ALWAYS prefixed with attributionPrefix when why is non-empty,
// and NEVER emitted at all when why is empty/whitespace-only (no dangling
// "The agent said:").
func attributedWhy(why string) string {
	if strings.TrimSpace(why) == "" {
		return ""
	}
	return attributionPrefix + sanitizeCardText(why)
}

// sanitizeCardText neutralizes untrusted text before it is embedded in a
// trusted, live-markup card field. Lead and Body are NOT Excerpt, so nothing
// downstream fences or inert-quotes them. Used for the agent's why (Body) and
// for the platform Reason (Lead) — the latter only looks platform-authored: on
// a VerifyRejected determination Reason is ProbeDetail, up to 200 bytes of the
// provider's raw HTTP response body.
//
// Three things unsanitized text could otherwise do:
//  1. Embed a newline to start what LOOKS like a second paragraph, posing as a
//     platform-authored verdict line under the real Lead. Collapsing every
//     whitespace run to one space makes this structurally impossible: the
//     result can never contain a line break, so it can never own a line.
//  2. Embed C0/C1 control characters (ANSI escapes included) to manipulate
//     terminal-rendered surfaces. Stripped outright rather than collapsed,
//     since none has a legitimate role in human-readable text.
//  3. Embed Unicode FORMAT characters (category Cf) — bidi overrides
//     U+202A-U+202E and U+2066-U+2069, plus zero-width joiners and spaces — to
//     reorder or hide text WITHIN the line. Line structure and the kind's own
//     escaping survive that; rendered reading order does not, so an attributed
//     block can be made to read as something the author never wrote. Dropped
//     alongside the controls. The cost — a ZWJ emoji sequence degrading to its
//     components, ZWNJ-dependent scripts losing a shaping hint — is acceptable
//     for a security card whose text is provider- and agent-supplied.
func sanitizeCardText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r):
			if !lastWasSpace {
				b.WriteRune(' ')
				lastWasSpace = true
			}
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			// dropped outright
		default:
			b.WriteRune(r)
			lastWasSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}
