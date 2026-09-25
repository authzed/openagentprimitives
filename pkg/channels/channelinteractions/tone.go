package channelinteractions

import "fmt"

// Tone is what KIND of concern a message carries. A fixed property of the
// category — "agent_failed" is always critical — so it lives on the registry
// row, not the per-request payload. A flow needing two tones is two
// categories, which is the point: tone stays reviewable in one file instead of
// being decided at each publish site.
//
// Tone is deliberately NOT an ordered severity, and the values are not ranked.
// Privacy is not "between" degraded and critical: a data-sharing decision is
// not WORSE than a tool approval, it is a different question, and how much it
// matters depends on whose data it is — which the reader knows and the system
// does not. Forcing that onto a red-to-blue ramp is what lets an
// information-disclosure message wear the same face as a status update.
//
// Tone says nothing about HOW a surface paints it. Slack maps it to a coloured
// emoji chip, the text floor to a marker word, the browser to a CSS class.
type Tone string

const (
	// ToneCritical — hostile input, or a session that is dead. The strongest
	// signal available, and it stays meaningful only because routine consent
	// does not share it.
	ToneCritical Tone = "critical"
	// TonePrivacy — this is about someone's data: who may see it, who already
	// did. Off the severity axis on purpose (see the type comment).
	TonePrivacy Tone = "privacy"
	// ToneDegraded — something failed, but the session survives and the user
	// can usually get what they wanted by trying again.
	ToneDegraded Tone = "degraded"
	// ToneWaiting — a PERSON is blocked on the reader. Held apart from
	// ToneRoutine for a social reason rather than a technical one: being the
	// bottleneck on a colleague costs something that being the bottleneck on a
	// process does not.
	ToneWaiting Tone = "waiting"
	// ToneRoutine — the agent needs permission to do something ordinary. The
	// most common tone in the system, which is exactly why it must not be
	// alarming.
	ToneRoutine Tone = "routine"
	// ToneHousekeeping — lifecycle churn the reader should be able to see but
	// need not act on. Deliberately the quietest tone.
	ToneHousekeeping Tone = "housekeeping"
	// ToneResolved — the thing the reader was waiting for happened. Its job is
	// to let them stop watching.
	ToneResolved Tone = "resolved"
	// ToneUnavailable — something the agent would have done is not available:
	// switched off, or outside what it can read at all. Nothing FAILED and
	// retrying cannot help; the remedy is always to change something — the
	// configuration, or the file — never to repeat the same request.
	//
	// Held apart from ToneDegraded, whose contract is "the user can usually
	// get what they wanted by trying again": that advice is actively wrong
	// here, and a tone that mis-states the remedy is worse than a quiet one.
	ToneUnavailable Tone = "unavailable"
)

// RequiresNextStep reports whether a category of this tone must say what the
// user should do about it — the single expression of the "never leave the user
// stuck" rule, which the conformance suite reads rather than re-listing tones.
//
// Enforced on NOTICES only: a prompt's buttons already answer "what do I do".
// The exempt tones are exempt because there is genuinely nothing to do —
// routine and waiting are answered by clicking, housekeeping reports something
// already settled, and resolved IS the resolution.
func (t Tone) RequiresNextStep() bool {
	switch t {
	case ToneCritical, TonePrivacy, ToneDegraded, ToneUnavailable:
		return true
	default:
		return false
	}
}

func (t Tone) valid() bool {
	switch t {
	case ToneCritical, TonePrivacy, ToneDegraded, ToneWaiting,
		ToneRoutine, ToneHousekeeping, ToneResolved, ToneUnavailable:
		return true
	default:
		return false
	}
}

// Glyph is an OPTIONAL semantic override for the mark a surface draws beside a
// message's lead, for the rare category whose meaning is better served by a
// specific symbol than by its tone colour.
//
// Use it sparingly, and never where tone or finality is what the reader needs:
// a glyph REPLACES the tone chip, so it costs both the colour and the shape. It
// belongs on a notice where neither is interesting — a cost report — never on a
// decision.
//
// It is deliberately SEMANTIC, never a per-channel symbol: a Slack emoji name
// here would put channel knowledge in the channel-agnostic registry, the
// `if kind == "slack"` anti-pattern in data form. Each renderer maps a Glyph to
// its own idiom and may ignore one it cannot draw.
//
// It is also a CLOSED, TYPED set rather than a string, because Slack accepts an
// unknown emoji name without error and renders it as literal `:name:` text — a
// free-text override would ship a typo silently.
type Glyph string

const (
	// GlyphDefault (the zero value) — draw the tone chip.
	GlyphDefault Glyph = ""
	// GlyphMoney — a cost or budget report, where neither the tone nor the
	// finality is what the reader came for.
	GlyphMoney Glyph = "money"
	// GlyphClock — waiting on time itself: an expiry, a scheduled retry.
	GlyphClock Glyph = "clock"
	// GlyphWarning — the request was understood and cannot be served for this
	// input. Louder than the quiet tones without claiming something broke, for
	// a notice whose tone is right but whose weight would otherwise read too
	// low. Spends the colour and the shape deliberately: for these, "can this
	// be served?" is what the reader needs, not "is this over?".
	GlyphWarning Glyph = "warning"
)

func (g Glyph) valid() bool {
	switch g {
	case GlyphDefault, GlyphMoney, GlyphClock, GlyphWarning:
		return true
	default:
		return false
	}
}

// validateNotice enforces the zero-action notice row shape, kept out of
// Category.Validate so the notice rules and the prompt rules read as two
// contracts rather than one interleaved one.
func (c Category) validateNotice() error {
	if c.Deciders != "" {
		return fmt.Errorf("notice category %q: must not declare a decider policy (a notice has no decision leg)", c.Name)
	}
	if c.Park != "" {
		return fmt.Errorf("notice category %q: must not park the session (a notice is not awaited)", c.Name)
	}
	if c.PendingCondition != "" {
		return fmt.Errorf("notice category %q: must not declare a pending condition (nothing is pending)", c.Name)
	}
	if c.Resurface != ResurfaceNone {
		return fmt.Errorf("notice category %q: must be ResurfaceNone (a delivered notice is not re-surfaced)", c.Name)
	}
	return nil
}

// validatePrompt enforces the prompt row shape.
func (c Category) validatePrompt() error {
	switch c.Deciders {
	case DecideOwner, DecideApprovers, DecideRequester, DecideParticipant, DecideResourceOwners, DecidePlatformAdmin:
	default:
		return fmt.Errorf("interaction category %q: unknown decider policy %q", c.Name, c.Deciders)
	}
	// Terminal means "nothing further will happen", which is unsayable about a
	// message that is waiting for the reader to act on it.
	if c.Terminal {
		return fmt.Errorf("interaction category %q: a prompt cannot be terminal (it is awaiting a decision)", c.Name)
	}
	return nil
}

// Valid reports whether t is a registered tone. Exported so guards can assert
// over AllTones without duplicating the switch.
func (t Tone) Valid() bool { return t.valid() }

// AllTones is the vocabulary, in ONE place, for every consumer that must
// handle every tone — chip tables, conformance guards, docs.
//
// Callers that hand-transcribe this list silently stop covering a tone the
// moment one is added, and an uncovered tone reaches a renderer's default:
// slack's toneChip defaults to the routine chip, so the failure mode is a
// message painted as the most common, least alarming thing in the system.
// Derive from here; never copy.
func AllTones() []Tone {
	return []Tone{
		ToneCritical, TonePrivacy, ToneDegraded, ToneWaiting,
		ToneRoutine, ToneHousekeeping, ToneResolved, ToneUnavailable,
	}
}

// Valid reports whether g is a registered glyph.
func (g Glyph) Valid() bool { return g.valid() }

// AllGlyphs is the glyph vocabulary, EXCLUDING GlyphDefault: the zero value
// means "draw the tone chip" and has no chip of its own, so including it would
// make every has-a-chip guard fail on it. Same derive-don't-transcribe
// reasoning as AllTones.
func AllGlyphs() []Glyph { return []Glyph{GlyphMoney, GlyphClock, GlyphWarning} }
