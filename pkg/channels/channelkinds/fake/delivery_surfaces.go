package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The fake kind ships with the smallest surface that lets a scenario exchange
// messages: text and markdown, and no live-view offer. That default is load
// bearing — a channel kind's Capabilities decide which meta tools an
// AgentClass is offered at all, so widening it would change the tool list of
// every bundle in the suite, and every assertion counting or naming tools with
// it.
//
// But it also means the fake transport cannot represent either way an artifact
// reaches a person: respond_to_user hides its `attached` field behind an
// asset:* capability, and artifact_offer_view asks the kind whether it has
// anywhere to render an offer. A harness that can express neither has a
// permanent blind spot exactly where a review report goes missing.
//
// So the surfaces are OPT-IN. A bundle that wants to exercise delivery turns
// them on for its own run and restores the default afterwards; every other
// bundle sees precisely what it saw before.
//
// # Why this is not the fixture proving itself
//
// Bundle.Trigger's doc rejects giving fake a trigger-status surface, and the
// distinction is worth stating because it looks like the same move. A status
// surface would need a third-party provider to report to, and fake has none;
// simulating one would test the simulation. A sub-channel SENDER has no such
// counterparty — its whole job is to render an envelope onto the transport, and
// recording what would have been rendered is what every other fake sender
// already does (permission_request, interaction, user_echo). What must not
// happen is the kind claiming a surface with no sender behind it: that would
// make artifact_offer_view report an offer as published while nothing recorded
// it, which is the very defect this exists to catch. SupportsLiveViewOffer and
// SubChannelSender therefore read the SAME flag, and a test asserts they agree
// in both states.

// deliverySurfaces is the process-wide opt-in. Atomic rather than mutex-guarded
// because it is written once per bundle and read on the runner's goroutines.
var deliverySurfaces atomic.Bool

// EnableDeliverySurfaces turns on the fake kind's artifact-delivery surfaces
// for the caller's run and returns the func that restores the default.
//
// Call the restore in a t.Cleanup, in the same discipline as ResetAllDrivers:
// bundles share one process, so a flag left on would silently widen the tool
// list of every scenario that ran after it — and a scenario passing because its
// predecessor enabled something is worse than one that never ran.
//
// Tests only. Nothing in production may call it: the flag is global, and a
// serving channelsd shares it with every Channel it hosts.
func EnableDeliverySurfaces() (restore func()) {
	prev := deliverySurfaces.Swap(true)
	return func() { deliverySurfaces.Store(prev) }
}

// DeliverySurfacesEnabled reports the current state. Exists so a test can
// assert the default rather than trusting it.
func DeliverySurfacesEnabled() bool { return deliverySurfaces.Load() }

// deliveryCapabilities are the capability strings the opt-in adds.
//
// text/html only: it is the one renderer kind an agent produces reports in, and
// a narrower list keeps the fake honest about being a fixture rather than a
// full transport.
var deliveryCapabilities = []string{"asset:text/html"}

// LiveViewOffer is a public projection of one captured KindLiveViewOffer
// envelope, so a bundle can assert the offer reached the transport rather than
// only that the tool said it had.
type LiveViewOffer struct {
	Payload    channelevents.LiveViewOfferPayload
	SessionRef channelkinds.SessionInfo
}

// recordedLiveViewOffer is the stored form.
type recordedLiveViewOffer struct {
	Payload    channelevents.LiveViewOfferPayload
	SessionRef channelkinds.SessionInfo
}

// LiveViewOffers returns a snapshot of every KindLiveViewOffer envelope this
// Driver's live_view_offer sender recorded. Append-only, like UserEchoes.
//
// Empty on a run that did not call EnableDeliverySurfaces: with the surface off
// the kind returns no sender, the relay drops the envelope, and nothing is
// recorded — which is the honest reflection of what that channel would do.
func (d *Driver) LiveViewOffers() []LiveViewOffer {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]LiveViewOffer, len(d.liveViewOffers))
	for i, p := range d.liveViewOffers {
		out[i] = LiveViewOffer{Payload: p.Payload, SessionRef: p.SessionRef}
	}
	return out
}

// fakeLiveViewOfferSender records KindLiveViewOffer envelopes onto the
// Driver's queue. Modeled on fakeUserEchoSender, with one difference: it
// resolves its Driver at SEND time rather than at construction.
//
// That is so asking a Kind whether it HAS a sender stays a safe question.
// Callers do ask — the channelsd conformance test checks every relayed kind's
// declaration against SubChannelSender(…, Deps{}) — and resolving a Driver from
// a zero Deps dereferences a nil Channel. A kind that panicked when asked would
// turn a declaration check into a crash.
type fakeLiveViewOfferSender struct {
	deps channelkinds.Deps
}

func (s *fakeLiveViewOfferSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if s.deps.Channel == nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake live_view_offer: sender has no Channel to record against")
	}
	if env.Kind != channelevents.KindLiveViewOffer {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake live_view_offer: unexpected envelope kind %q", env.Kind)
	}
	var pl channelevents.LiveViewOfferPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake live_view_offer: decode LiveViewOfferPayload: %w", err)
	}
	if pl.ArtifactID == "" {
		// Same refusal every real live-view sender makes. An offer naming no
		// artifact would record a delivery of nothing.
		return channelkinds.SubChannelSendResult{}, fmt.Errorf("fake live_view_offer: payload missing artifactId")
	}
	drv := driverFor(s.deps.Channel)
	drv.mu.Lock()
	drv.liveViewOffers = append(drv.liveViewOffers, recordedLiveViewOffer{Payload: pl, SessionRef: sess})
	drv.mu.Unlock()
	return channelkinds.SubChannelSendResult{}, nil
}
