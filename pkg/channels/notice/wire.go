package notice

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// ToWire flattens a notice into its serialisable form for a boundary the rich
// type cannot cross — an HTTP response to the web chat, a NATS reply to a view
// surface, the CLI's TUI.
//
// Tone and Glyph are resolved from the registry HERE, at the boundary, and
// denormalised onto the wire: a receiver on the far side of an HTTP response
// has no category registry (the browser certainly does not), so a wire notice
// carrying only a category name would be undrawable. Carrying both keeps the
// registry authoritative for producers while letting any consumer render.
//
// A suppressed or nil notice returns nil: there is nothing to draw, and the
// caller is expected to have logged SuppressReason.
func (n *Notice) ToWire() *channelevents.NoticeWire {
	if n.IsSuppressed() {
		return nil
	}
	w := &channelevents.NoticeWire{
		Category: n.category,
		Lead:     n.args.Lead,
		Body:     n.args.Body,
		NextStep: n.args.NextStep,
		Fields:   n.args.Fields,
		Excerpt:  n.args.Excerpt,
	}
	// A category that fails to resolve still produces a drawable notice: tone
	// is a presentation hint, and losing it must never be the reason a user
	// hears nothing. The producer-side error surfaces from Payload, where it
	// can still be acted on.
	if tone, terminal, glyph, err := n.Style(); err == nil {
		w.Tone = string(tone)
		w.Terminal = terminal
		w.Glyph = string(glyph)
	}
	return w
}

// FromWire rebuilds a notice from its wire form, for a consumer that received
// one and wants to render it through the same helpers a producer uses.
//
// The rebuilt notice is renderable but NOT re-publishable: Audience does not
// cross the wire (the receiving surface IS the audience), so Payload on the
// result fails validation. Deliberate — a notice is published once, by the
// component that decided it; letting a consumer round-trip one back onto the
// bus would make the origin of a user-facing message unknowable.
func FromWire(w *channelevents.NoticeWire) *Notice {
	if w.IsZero() {
		return nil
	}
	return &Notice{
		category: w.Category,
		args: Args{
			Lead:     w.Lead,
			Body:     w.Body,
			NextStep: w.NextStep,
			Fields:   w.Fields,
			Excerpt:  w.Excerpt,
		},
	}
}

// WireTone reads a wire notice's tone without needing the category registry —
// which a consumer on the far side of an HTTP response does not have.
func WireTone(w *channelevents.NoticeWire) channelinteractions.Tone {
	if w == nil {
		return channelinteractions.ToneRoutine
	}
	return channelinteractions.Tone(w.Tone)
}
