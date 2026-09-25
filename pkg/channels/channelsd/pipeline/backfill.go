package pipeline

import (
	"context"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// initialBackfillLimit caps the messages seeded at adoption. A longer
// thread is truncated newest-kept; the agent reaches the rest via the
// read_thread_history tool.
const initialBackfillLimit = 50

// HistoryReadFunc fetches prior channel messages for adoption backfill
// and catch-up. The pipeline holds it as a nil-able func (not an
// interface) so a nil value is an honest "no backfiller wired" — and so
// pipeline unit tests can stub it without any Slack code. internal/cmd/channelsd
// wires the real implementation (resolve the kind, type-assert
// ConversationReader, call ReadHistory).
type HistoryReadFunc func(
	ctx context.Context,
	ch *spiceboxv1alpha1.Channel,
	channelKey string,
	opts channelkinds.ReadHistoryOpts,
) (channelkinds.HistoryPage, error)

// formatTranscript renders history messages into a single attributed
// block suitable for one memory turn. truncated prepends an
// omitted-messages note that primes the agent to use read_thread_history.
func formatTranscript(header string, msgs []channelkinds.HistoryMessage, truncated bool) string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(header)
	b.WriteString("]\n")
	if truncated {
		b.WriteString("[earlier messages omitted — use read_thread_history to fetch more]\n")
	}
	for _, m := range msgs {
		name := m.AuthorDisplayName
		if name == "" {
			name = m.AuthorExternalID
		}
		if name == "" {
			name = "unknown"
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(m.Text)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// distinctParticipants returns the first HistoryMessage seen for each
// distinct non-app author, in first-seen order. Used to derive the
// interact-grant roster.
func distinctParticipants(msgs []channelkinds.HistoryMessage) []channelkinds.HistoryMessage {
	seen := map[string]bool{}
	out := make([]channelkinds.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.FromApp || m.AuthorExternalID == "" || seen[m.AuthorExternalID] {
			continue
		}
		seen[m.AuthorExternalID] = true
		out = append(out, m)
	}
	return out
}

// inheritedAnnotations builds the annotation map for a newly-created
// session. It always stamps the started-by external id, the started-by
// canonical subject ("user:<canonical>"), and the started-by verified email
// (empty when the channel kind has none — see AnnotationStartedByEmail).
// A session inheriting from an archived predecessor also carries forward the
// two backfill cursors, so catch-up resumes where the predecessor left off
// instead of re-backfilling.
func inheritedAnnotations(startedByExternalID, startedByCanonicalSubject, startedByEmail string, archived *spiceboxv1alpha1.AgentSession) map[string]string {
	ann := map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID:  startedByExternalID,
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: startedByCanonicalSubject,
		spiceboxv1alpha1.AnnotationStartedByEmail:       startedByEmail,
	}
	if archived == nil {
		return ann
	}
	for _, k := range []string{
		spiceboxv1alpha1.AnnotationBackfilledFromTS,
		spiceboxv1alpha1.AnnotationBackfilledThroughTS,
	} {
		if v := archived.Annotations[k]; v != "" {
			ann[k] = v
		}
	}
	return ann
}

// adoptionRoutingMode returns the RoutingMode for a newly-created session's
// binding. A thread being adopted now is mention_only; a session inheriting
// from an archived predecessor copies the predecessor's mode; otherwise "".
func adoptionRoutingMode(adopting bool, archived *spiceboxv1alpha1.AgentSession) string {
	if adopting {
		return "mention_only"
	}
	if archived != nil && archived.Spec.InputChannel != nil {
		return archived.Spec.InputChannel.RoutingMode
	}
	return ""
}

// fetchAdoptionHistory fetches the bounded backfill window for a thread
// being adopted. Best-effort: a ReadHistory error yields an empty page
// (logged) and the session starts without backfill.
func (p *Pipeline) fetchAdoptionHistory(ctx context.Context, ev channelkinds.InboundEvent) channelkinds.HistoryPage {
	page, err := p.ReadHistory(ctx, ev.Channel, ev.ChannelKey, channelkinds.ReadHistoryOpts{
		BeforeTS: ev.External["message_ts"],
		Limit:    initialBackfillLimit,
	})
	if err != nil {
		log.FromContext(ctx).Info("thread adoption: ReadHistory failed; starting session without backfill",
			"channelKey", ev.ChannelKey, "err", err.Error())
		return channelkinds.HistoryPage{}
	}
	return page
}

// adoptionPrompt prepends the backfilled thread transcript to the live
// mention so the new session's spec.Prompt carries history-then-question
// in order. The new-session path delivers the first turn via spec.Prompt
// (not memory): on cold start the runner replays spec.Prompt as turn 0
// and drains channelsd "inbox" turns AFTER it, so seeding the transcript
// into memory instead would place it after the question. Keeping the
// transcript in the prompt guarantees the agent reads history first.
func adoptionPrompt(liveMention string, page channelkinds.HistoryPage) string {
	if len(page.Messages) == 0 {
		return liveMention
	}
	return formatTranscript("Thread history before you were invited", page.Messages, page.HasMore) +
		"\n\n" + liveMention
}

// applyAdoptionGrants writes interact_participant for each distinct thread
// author and stamps RoutingMode + the two backfill cursors on the freshly-
// created session. Returns the display names of the authors who ended up able
// to interact, and of those the class's declared interact policy withheld —
// both for the join notice.
//
// Best-effort per the no-silent-errors rule: a SpiceDB write failure for
// one participant is logged and skipped; the others still land.
// threadSeedRequestsFor builds the per-slot seeding inputs from the class:
// which slots opt into channel_thread, whose contributions they trust, and the
// value→id chain the reconciler derived from the tools.
//
// A slot with no published ValueTransforms is still passed through —
// SeedFromThread refuses it on its own, because "nothing identifies the shape
// of these values" is its decision to make, not this adapter's.
//
// AutoGrantFrom is handed over verbatim. Rebuilding the slice would erase
// nil-versus-empty, and that distinction is the difference between "trust the
// owner" and "trust nobody".
func threadSeedRequestsFor(class *spiceboxv1alpha1.AgentClass) []authz.ThreadSeedRequest {
	slots := class.Spec.GetSlots()
	if len(slots) == 0 {
		return nil
	}
	transforms := make(map[string][]string, len(class.Status.ResolvedSlots))
	for _, rs := range class.Status.ResolvedSlots {
		transforms[rs.ResourceType] = rs.ValueTransforms
	}
	var out []authz.ThreadSeedRequest
	for _, s := range slots {
		if !authz.AllowsFill(s.FillFrom, authz.FillChannelThread) {
			continue
		}
		out = append(out, authz.ThreadSeedRequest{
			ResourceType:    s.ResourceType,
			Permission:      s.Permission,
			ValueTransforms: transforms[s.ResourceType],
			AutoGrantFrom:   s.AutoGrantFrom,
		})
	}
	return out
}

// maxThreadSeededBindings caps how many instances one adopted thread may bind
// without any human involvement. A long thread can carry a great many links,
// and every one of them would otherwise become standing reach for the session.
// Anything past the cap is not dropped silently — it is logged, and the values
// still work through the ordinary approval flow.
const maxThreadSeededBindings = 32

// seedThreadSlots binds the instances a trusted author already put in the
// thread, so the agent may act on the links the conversation is ABOUT without
// prompting for each one — while a value nobody trusted still Check-fails and
// raises an approval.
//
// Best-effort by construction: every failure here costs an approval prompt, and
// none of them can grant anything that was not going to be granted.
func (p *Pipeline) seedThreadSlots(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
	page channelkinds.HistoryPage,
	requester identity.CanonicalUserID,
	reqs []authz.ThreadSeedRequest,
	expiresAt time.Time,
) {
	if len(reqs) == 0 || len(page.Messages) == 0 {
		return
	}
	logger := log.FromContext(ctx)

	msgs := make([]authz.ThreadMessage, 0, len(page.Messages))
	for _, m := range page.Messages {
		if m.FromApp {
			continue // the agent's own posts are not a trust source
		}
		// An author who cannot be canonicalised is left with an empty subject,
		// which SeedFromThread treats as untrusted rather than skipping — the
		// distinction matters under autoGrantFrom: [participants].
		canonical, cerr := identity.FromExternal(
			identity.Kind(ev.ExternalIDs.Kind),
			identity.TeamScope(ev.ExternalIDs.TeamScope),
			identity.RawExternalID(m.AuthorExternalID),
			identity.Email(m.AuthorEmail),
		).AllowSynthetic().Canonical()
		if cerr != nil {
			logger.Info("thread slot seeding: author not canonicalisable; their values are untrusted",
				"session", sess.Namespace+"/"+sess.Name, "author", m.AuthorExternalID, "err", cerr.Error())
			canonical = identity.CanonicalUserID{}
		}
		msgs = append(msgs, authz.ThreadMessage{AuthorCanonical: canonical.String(), Text: m.Text})
	}

	bindings, dropped := authz.SeedFromThread(msgs, requester.String(), reqs, maxThreadSeededBindings)
	if dropped > 0 {
		logger.Info("thread slot seeding: capped; the rest still work through approval",
			"session", sess.Namespace+"/"+sess.Name,
			"bound", len(bindings), "dropped", dropped, "cap", maxThreadSeededBindings)
	}
	if len(bindings) == 0 {
		return
	}
	if err := p.Authz.GrantSlots(ctx, sess.Namespace, sess.Name, bindings, expiresAt); err != nil {
		// Not fatal: without the grants the agent simply has to ask.
		logger.Info("thread slot seeding: grant write failed; those values will need approval",
			"session", sess.Namespace+"/"+sess.Name, "bindings", len(bindings), "err", err.Error())
		return
	}
	logger.Info("thread slot seeding: bound values a trusted author put in the thread",
		"session", sess.Namespace+"/"+sess.Name, "bindings", len(bindings))
}

func (p *Pipeline) applyAdoptionGrants(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
	page channelkinds.HistoryPage,
	interactPolicy string,
) (granted, withheld []string) {
	logger := log.FromContext(ctx)

	// Grant interact to every distinct thread author — UNLESS the class
	// declares an interact policy, in which case that policy decides.
	//
	// Adoption used to ignore the policy entirely, so a class stating "only eng
	// may interact" still handed durable standing to whoever had posted in the
	// thread. interact is not nominal: it confers memory_entry#read and
	// artifact#view on the session, and slot grants resolve through it.
	//
	// The policy subject is written as a participant before this runs, so an
	// author the policy admits ALREADY passes the interact check and needs no
	// tuple of their own; one it does not admit gets none.
	for _, m := range distinctParticipants(page.Messages) {
		// Thread authors may be guests / foreign-workspace users with no
		// verified email — keyed by the synthetic subject exactly as their
		// inbound messages are, so opt into synthetic. With the opt-in this
		// never errors; skip defensively if it somehow does.
		canonical, cerr := identity.FromExternal(
			identity.Kind(ev.ExternalIDs.Kind),
			identity.TeamScope(ev.ExternalIDs.TeamScope),
			identity.RawExternalID(m.AuthorExternalID),
			identity.Email(m.AuthorEmail),
		).AllowSynthetic().Canonical()
		if cerr != nil {
			logger.Info("thread adoption: canonicalize participant failed; skipped",
				"session", sess.Namespace+"/"+sess.Name, "participant", m.AuthorExternalID, "err", cerr.Error())
			continue
		}
		name := m.AuthorDisplayName
		if name == "" {
			name = m.AuthorExternalID
		}

		if interactPolicy != "" {
			ref := authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}
			// FullyConsistent: the policy participant was written moments ago on
			// this same request, so a cached snapshot could miss it and withhold
			// from someone the policy plainly admits.
			ok, cerr := p.Engine.CheckSessionInteract(ctx, ref, canonical, true)
			if cerr != nil {
				// Fail CLOSED. Failing open here would let a SpiceDB blip
				// silently restore bulk-granting for exactly the classes that
				// took the trouble to declare a policy.
				logger.Info("thread adoption: interact policy check failed; withholding grant",
					"session", sess.Namespace+"/"+sess.Name, "participant", m.AuthorExternalID,
					"policy", interactPolicy, "err", cerr.Error())
				withheld = append(withheld, name)
				continue
			}
			if !ok {
				logger.Info("thread adoption: author is outside the declared interact policy; no grant",
					"session", sess.Namespace+"/"+sess.Name, "participant", m.AuthorExternalID,
					"policy", interactPolicy)
				withheld = append(withheld, name)
				continue
			}
			// Admitted by the policy — already holds interact, so writing a
			// per-user tuple would only duplicate standing they have.
			granted = append(granted, name)
			continue
		}

		if werr := p.Engine.TouchInteractParticipantUser(ctx, authz.SessionRef{Namespace: sess.Namespace, Name: sess.Name}, canonical); werr != nil {
			logger.Info("thread adoption: interact grant failed for one participant; skipped",
				"session", sess.Namespace+"/"+sess.Name, "participant", m.AuthorExternalID, "err", werr.Error())
			continue
		}
		granted = append(granted, name)
	}

	// Stamp RoutingMode (defensive — already set at create time) and the
	// two backfill cursors.
	patched := sess.DeepCopy()
	if patched.Spec.InputChannel != nil {
		patched.Spec.InputChannel.RoutingMode = "mention_only"
	}
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[spiceboxv1alpha1.AnnotationBackfilledFromTS] = page.Messages[0].TS
	patched.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS] = ev.External["message_ts"]
	if perr := p.K8s.Patch(ctx, patched, client.MergeFrom(sess)); perr != nil {
		logger.Info("thread adoption: failed to stamp RoutingMode/cursors",
			"session", sess.Namespace+"/"+sess.Name, "err", perr.Error())
		return granted, withheld
	}
	*sess = *patched
	return granted, withheld
}

// catchUp seeds the messages a mention_only session missed since its
// last turn. It runs on every mention to an adopted thread, after the
// permission check passes and before the live message is appended. It
// fetches messages after AnnotationBackfilledThroughTS and before the
// triggering message, drops app messages and the trigger itself, seeds
// the remainder as one transcript turn, and advances the cursor.
//
// It grants NO interact: the auto-interact roster is frozen at adoption.
// Best-effort throughout — a failure logs and leaves the session usable.
func (p *Pipeline) catchUp(
	ctx context.Context,
	sess *spiceboxv1alpha1.AgentSession,
	ev channelkinds.InboundEvent,
) {
	logger := log.FromContext(ctx)
	triggerTS := ev.External["message_ts"]
	through := sess.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS]

	page, err := p.ReadHistory(ctx, ev.Channel, ev.ChannelKey, channelkinds.ReadHistoryOpts{
		AfterTS:  through,
		BeforeTS: triggerTS,
	})
	if err != nil {
		logger.Info("thread catch-up: ReadHistory failed; proceeding with the live message only",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return
	}

	delta := make([]channelkinds.HistoryMessage, 0, len(page.Messages))
	for _, m := range page.Messages {
		// Skip THIS agent's own messages, the triggering message itself, and
		// anything at or before the cursor — conversations.replies always
		// returns the thread parent regardless of the Oldest bound, which would
		// otherwise re-seed the root on every catch-up.
		//
		// The skip is FromSelf, not FromApp: the agent's own replies are
		// already in its memory and re-seeding them would double every turn,
		// but a THIRD-PARTY app — an alerting webhook, a CI bot — is posting
		// content the agent has never seen. Dropping those was how alerts that
		// fired mid-thread vanished entirely.
		if m.FromSelf || m.TS == triggerTS || (through != "" && m.TS <= through) {
			continue
		}
		delta = append(delta, m)
	}
	if len(delta) > 0 {
		transcript := formatTranscript("New messages in the thread since your last reply", delta, false)
		if aerr := p.Memory.Append(ctx, sess.Namespace, sess.Name, MemTurn{
			Role:    "user",
			Content: []MemContent{{Type: "text", Text: transcript}},
		}); aerr != nil {
			logger.Info("thread catch-up: transcript append failed",
				"session", sess.Namespace+"/"+sess.Name, "err", aerr.Error())
		}
	}

	// Advance the cursor to the triggering message regardless of delta
	// size — the next catch-up should start after this turn.
	patched := sess.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS] = triggerTS
	if perr := p.K8s.Patch(ctx, patched, client.MergeFrom(sess)); perr != nil {
		logger.Info("thread catch-up: failed to advance cursor",
			"session", sess.Namespace+"/"+sess.Name, "err", perr.Error())
	}
}
