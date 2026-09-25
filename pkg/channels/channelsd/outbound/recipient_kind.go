package outbound

import (
	"encoding/json"
	"slices"

	"github.com/go-logr/logr"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// restampAudienceKinds re-stamps the channel-kind tag on a human-directed
// card's audience identities with the kind of the binding the relay actually
// resolved the card onto.
//
// The tag says which surface a person is reachable on, and a SURFACE knows it
// while a PUBLISHER can only guess. The runner stamps
// Loop.AddressableChannelKind() — its own session's outbound binding — and
// sessionhold stamps sess.Spec.InputChannel.Kind. Both were right while every
// outbound binding was one a human read. A conversational subagent's binding is
// an `agent` Channel whose far side is its parent session, so for one of those
// both publishers stamp "agent" onto an identity naming a PERSON, and the card
// is then routed by the lineage walk to an ancestor's Slack. Slack's
// interaction sender skips every recipient whose kind is not "slack"
// (interaction.go), delivered stays false, reportUndeliverable fires, and the
// child parks awaiting a decision nobody was ever asked for.
//
// That is the same failure the comment at internal/cmd/runner/main.go's
// OutboundChannelKind assignment records from cron sessions, reached by a new
// route: the earlier fix moved the stamp from the INPUT binding to the OUTBOUND
// one, which only worked because an outbound binding was assumed human-readable.
//
// The correction lives here, not in the publishers, for two reasons:
//
//   - The publishers cannot compute the answer. The runner's Role grants `get`
//     on agentsessions pinned to its OWN session name
//     (pkg/controllers/agentsession/rbac.go), so it cannot walk its lineage to
//     find the delivering binding even if it wanted to; and a stamp resolved at
//     pod start could drift from where a card is routed minutes later.
//   - It fixes every publisher and every surface at once. Slack alone gates on
//     the tag in three places — the recipient fan-out (`r.Kind != "slack"`),
//     the public note (whose named approvers ARE the delivered recipients), and
//     a field's Mentions (resolveFieldMentions' `m.Kind != "slack"`, which
//     falls back to inert display text). This rewrites all three audience-
//     identity sites the payload carries, so one correction covers them; a
//     per-sender rule would have to be repeated in each, and again in every
//     kind added later.
//
// Only identities whose stamped kind CANNOT reach a person are rewritten. A
// tag naming a real human surface is left exactly as the publisher wrote it,
// even when it differs from the resolved binding: that is a genuine
// cross-surface identity, not this defect. A tag deliversToHuman cannot answer
// for (an unregistered value such as the "idp" viewer identity a proxy-exec
// approval carries) is also left alone — the publisher meant something
// specific and guessing at it is how a second defect gets introduced under
// cover of fixing this one.
//
// WHAT MUST BE TRUE FOR A RESTAMP TO BE SAFE. The tag is addressing metadata
// and not identity — but only for the two identity shapes whose subject does
// not read it. ExternalIdentity.Principal() passes a Subject through verbatim,
// and canonicalizes an identity carrying an Email to base64url(email); neither
// derivation looks at Kind, so rewriting the tag cannot move WHO the surface
// resolves. The third shape — Kind + ExternalID, no Email and no Subject —
// canonicalizes to base64url(kind:teamScope:externalID) under AllowSynthetic,
// so its subject DOES move with the tag, and slack's
// resolveSlackUserIDFromCanonical then returns the externalID out of a
// "slack:<team>:<id>" canonical as a Slack user id, without verifying that id
// exists. Rewriting that shape is correct only while the raw id is already
// native to the resolved kind, and today it is: every publisher that can emit
// it takes the raw id from the session's started-by annotations — the runner's
// non-proxy tool-approval requester (pipeline_wiring.go), its identity-choice
// requester (identitygate.go), and channelsd's credential-update recipient
// when the author IS the starter — so the id was minted by the surface the
// starting human arrived on, and a delegated child inherits it verbatim. Every
// other publisher of an audience identity carries a Subject or an Email.
//
// What this does NOT promise is the general case: an email-less,
// subject-less identity whose raw id was minted by a different surface family
// than the one the walk resolves onto would be re-tagged onto an id space that
// never issued it. The guard is the identity's shape, not the walk — carry an
// Email or a Subject and the question cannot arise.
// recipient_kind_test.go pins the derived-subject move, so a change to the
// canonical encoding cannot leave this paragraph quietly stale.
//
// Returns env unchanged when nothing needs rewriting, when the payload does not
// decode, or when the rewritten payload does not re-marshal. A card delivered
// with a stale tag is the behaviour that already exists; a dropped card is not,
// so neither failure is allowed to escalate. Both are logged.
// stampAskingSession overwrites InteractionRequestPayload.AgentSessionRef with
// the identity the SUBJECT authorized, for every interaction request.
//
// AgentSessionRef is publisher-controlled JSON that its own doc calls "a
// denormalized copy for renderer convenience, not validated here" — and nothing
// validated it. Envelope.Session is publisher-controlled too, but the relay has
// already dropped this envelope unless it matched the subject the publisher's
// per-session JWT authorizes, so by here it is checked. Renderers read the
// payload copy, so the checked value has to be put there.
//
// This matters because renderers use it to say WHO IS ASKING, and a card about
// a delegated child is routed up the lineage to a person who is not watching
// that session. The dangerous direction is SUPPRESSION, not spoofing: the
// client-hosted TUI shows the line only when the asking session differs from
// the one on screen, so a runner that wrote the WATCHED session's name into
// this field would make the attribution vanish exactly when the approver most
// needs it. Slack's provenance footer is misled the milder way, printing a
// session that never asked.
//
// Overwrite, never merge or fill-if-empty: a publisher that supplied a value
// is precisely the case being corrected.
//
// Same shape and same reason as restampAudienceKinds below — the relay is
// where publisher-supplied values get reconciled against what the platform
// actually knows.
func stampAskingSession(env channelevents.Envelope, logger logr.Logger) channelevents.Envelope {
	if env.Kind != channelevents.KindInteractionRequest {
		return env
	}
	var pl channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		logger.Info("outbound relay: interaction payload did not decode; leaving the asking session as published",
			"err", err.Error())
		return env
	}
	if pl.AgentSessionRef == env.Session {
		return env
	}
	if pl.AgentSessionRef.Name != "" {
		// Worth a line: the only way to reach this is a publisher naming a
		// session other than the one its subject authorizes.
		logger.Info("outbound relay: interaction request claimed a different asking session than its subject; correcting",
			"claimedAsker", pl.AgentSessionRef.Namespace+"/"+pl.AgentSessionRef.Name,
			"subjectSession", env.Session.Namespace+"/"+env.Session.Name)
	}
	pl.AgentSessionRef = env.Session
	raw, err := json.Marshal(pl)
	if err != nil {
		logger.Info("outbound relay: re-encoding the interaction payload failed; leaving it as published",
			"err", err.Error())
		return env
	}
	env.Payload = raw
	return env
}

// stampAskingChain records the delegation path a human-directed card travelled
// to reach its reader.
//
// Separate from stampAskingSession, and later, because the two facts become
// known at different moments: the asker is settled by the subject cross-check
// at the top of handle, while the chain only exists once the lineage walk has
// run. Stamping both at the first point would mean stamping a chain nobody had
// computed yet.
//
// A chain of one hop means the card is being delivered through the asking
// session's own binding — there is no relationship to explain — so it is
// cleared rather than written. Clearing matters as much as writing: a publisher
// that pre-populated the field must not have a stale claim survive just because
// this delivery had nothing to say.
func stampAskingChain(env channelevents.Envelope, chain []spiceboxv1alpha1.NamespacedRef, logger logr.Logger) channelevents.Envelope {
	if env.Kind != channelevents.KindInteractionRequest {
		return env
	}
	var pl channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		logger.Info("outbound relay: interaction payload did not decode; leaving the asking chain as published",
			"err", err.Error())
		return env
	}
	var want []channelevents.SessionRef
	if len(chain) > 1 {
		want = make([]channelevents.SessionRef, 0, len(chain))
		for _, ref := range chain {
			want = append(want, channelevents.SessionRef{Namespace: ref.Namespace, Name: ref.Name})
		}
	}
	if slices.Equal(pl.AskingChain, want) {
		return env
	}
	pl.AskingChain = want
	raw, err := json.Marshal(pl)
	if err != nil {
		logger.Info("outbound relay: re-encoding the interaction payload failed; leaving the asking chain as published",
			"err", err.Error())
		return env
	}
	env.Payload = raw
	return env
}

func restampAudienceKinds(
	env channelevents.Envelope,
	resolvedKind string,
	deliversToHuman func(kindName string) (bool, error),
	logger logr.Logger,
) channelevents.Envelope {
	// Only the request carries an audience to address. InteractionApplied and
	// InteractionDecisionRejected — the other two human-directed kinds — name a
	// decider and a clicker, neither of which is a delivery recipient list.
	if env.Kind != channelevents.KindInteractionRequest || resolvedKind == "" {
		return env
	}
	var pl channelevents.InteractionRequestPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		logger.Info("outbound relay: interaction payload did not decode; leaving recipient kinds as published",
			"err", err.Error())
		return env
	}

	changed := false
	restamp := func(e *channelevents.ExternalIdentity) {
		if e == nil || !needsRestamp(e.Kind, deliversToHuman) {
			return
		}
		e.Kind = identity.Kind(resolvedKind)
		changed = true
	}
	restamp(pl.Audience.Requester)
	for i := range pl.Audience.Approvers {
		restamp(&pl.Audience.Approvers[i])
	}
	// A field's Mentions name PARTIES rather than the delivery audience —
	// info_leakage's "Would share with" recipients are the case that matters —
	// and the runner stamps them from h.l.ChannelKind, its INPUT binding, which
	// for a conversational child is `agent` too. Left unstamped they hit
	// resolveFieldMentions' non-slack branch and render as inert display text,
	// on the one category whose whole point is that the data owner sees exactly
	// who the share reaches.
	for i := range pl.Fields {
		for j := range pl.Fields[i].Mentions {
			restamp(&pl.Fields[i].Mentions[j])
		}
	}
	if !changed {
		return env
	}

	// Round-tripped through the payload type rather than patched as raw JSON:
	// the type IS this payload's schema on both sides of the bus (the same
	// struct the publisher marshalled), so a field it does not carry cannot
	// have been present to lose.
	raw, err := json.Marshal(pl)
	if err != nil {
		logger.Info("outbound relay: re-stamped interaction payload did not marshal; delivering as published",
			"requestRef", pl.RequestRef, "err", err.Error())
		return env
	}
	logger.Info("outbound relay: re-stamped human-directed audience identities onto the delivering channel kind",
		"requestRef", pl.RequestRef, "category", pl.Category, "resolvedKind", resolvedKind)
	env.Payload = raw
	return env
}

// needsRestamp reports whether an audience identity's channel-kind tag names a
// surface no person reads — an empty tag, or a registered kind whose
// DeliversToHuman is false. An error from the predicate means the tag is not a
// channel kind this process knows, which is not this function's to correct.
func needsRestamp(k identity.Kind, deliversToHuman func(kindName string) (bool, error)) bool {
	if k == "" {
		return true
	}
	human, err := deliversToHuman(k.String())
	if err != nil {
		return false
	}
	return !human
}
