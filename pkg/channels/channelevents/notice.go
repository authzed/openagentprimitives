package channelevents

// NoticeWire is the serialisable form of a one-way user-facing notice: what
// crosses an HTTP or NATS boundary when the rich in-process *notice.Notice
// cannot.
//
// It lives here, not in pkg/channels/notice, for the same reason ExternalIdentity is
// mirrored rather than shared: pkg/channels/notice imports pkg/channels/channelevents (it builds
// InteractionRequestPayload), so channelevents cannot import back. Wire types
// live at the bottom of the dependency graph.
//
// Tone and Glyph are carried as plain strings rather than the
// channelinteractions enums for the same reason. A consumer that wants the
// typed values resolves the category from its own registry; a consumer that
// only wants to draw a card (the web chat, the TUI) reads these directly and
// needs no registry at all.
type NoticeWire struct {
	// Category is the registry key, so a consumer that HAS the registry can
	// recover everything else.
	Category string `json:"category,omitempty"`
	// Tone is what kind of concern this carries (critical, privacy, degraded,
	// waiting, routine, housekeeping, resolved). Denormalised from the
	// category so a surface can pick a colour without a registry lookup.
	Tone string `json:"tone,omitempty"`
	// Terminal reports that nothing further will happen. Surfaces draw it
	// distinctly so "stop waiting" is legible without reading the copy.
	Terminal bool `json:"terminal,omitempty"`
	// Glyph is the optional semantic mark override ("lock", "clock", …).
	// Empty means "draw whatever the severity implies".
	Glyph string `json:"glyph,omitempty"`

	// Lead is the one-sentence headline; empty means there is nothing to draw
	// at all (see IsZero).
	Lead string `json:"lead"`
	// Body is optional supporting detail. Publisher-authored trusted copy.
	Body string `json:"body,omitempty"`
	// NextStep is the one imperative sentence saying what the reader should
	// DO. Required for the tones Tone.RequiresNextStep names.
	NextStep string `json:"nextStep,omitempty"`

	// Fields are label/value supporting rows.
	Fields []InteractionField `json:"fields,omitempty"`
	// Excerpt is UNTRUSTED content. Every consumer MUST render it inert —
	// fenced, escaped, never as live markup — exactly as the interaction
	// renderers do. Crossing a wire does not launder it.
	Excerpt *InteractionExcerpt `json:"excerpt,omitempty"`
}

// Text renders the notice as the plain-text one-liner a surface uses when it
// has no structure at all — a log line, a push preview, a terminal that is
// not drawing a card.
//
// It deliberately joins lead and next step and nothing else: those are the two
// things a stuck user needs, and everything below them is detail a
// single-line surface has no room for.
func (n *NoticeWire) Text() string {
	if n == nil {
		return ""
	}
	if n.NextStep == "" {
		return n.Lead
	}
	return n.Lead + " — " + n.NextStep
}

// IsZero reports whether there is nothing to draw.
//
// It means exactly that, and nothing else. A DELIBERATE silence is not
// represented here at all: suppression is decided upstream by
// notice.Suppressed, which carries a reason for the log, and a suppressed
// notice simply never reaches a wire. Keeping the two distinct is what
// prevents a dropped message from being indistinguishable from an intended
// one.
func (n *NoticeWire) IsZero() bool { return n == nil || n.Lead == "" }
