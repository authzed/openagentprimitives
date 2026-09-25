// Package notice is the authoring seam for one-way user-facing messages:
// errors, denials, warnings, degradation notices, lifecycle events —
// everything the system says to a user that is not agent speech, not tool
// output, and not an approval prompt.
//
// A notice IS an interaction with zero actions. It rides
// channelevents.KindInteractionRequest, is described by a
// channelinteractions.Category row with Notice: true, and is rendered by the
// same per-channel machinery that renders prompts. There is deliberately no
// second model, no second registry, and no second renderer: one wire kind and
// one renderer per surface keeps every one-way message consistent without any
// call site having to know what consistency looks like.
//
// A caller supplies the COPY (lead, body, what to do about it); the registry
// supplies the TONE (and optional glyph). A caller cannot choose a colour or a
// symbol, which is what keeps the vocabulary closed.
package notice

import (
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// Args is the per-call copy of a notice. Everything here is publisher-authored
// and TRUSTED: surfaces render Lead, Body and NextStep as live markup.
// Externally-derived text — a failure reason, a stack trace, tool output —
// MUST travel in Excerpt (rendered inert) or Details (behind a click), never
// interpolated into the trusted fields.
type Args struct {
	// Lead is the headline: what happened, in one short clause, in the
	// user's terms. Required.
	Lead string
	// Body explains it. Optional but near-always wanted.
	Body string
	// NextStep is one imperative sentence telling the user what to do.
	// Required for the tones channelinteractions.Tone.RequiresNextStep names —
	// a notice at those tones that names no action leaves the user stuck,
	// which is the defect this field exists to make impossible.
	NextStep string
	// Fields are label/value rows for structured supporting detail.
	Fields []channelevents.InteractionField
	// Excerpt carries UNTRUSTED content shown for human judgement. Rendered
	// inert by every surface.
	Excerpt *channelevents.InteractionExcerpt
	// Details is the diagnostic payload behind the generic Show-Details
	// button — a stack trace, a raw condition message. Its presence is what
	// makes a surface offer the button at all.
	Details json.RawMessage
	// Audience says who sees it. Required.
	Audience channelevents.InteractionAudience
}

// Notice is a built one-way message: a category plus its copy, or an explicit
// decision to say nothing. It is immutable once built.
//
// Build errors are carried rather than returned by New, so call sites read as
// declarations rather than error-handling ceremony; the error surfaces at
// Payload/Publish, where a caller is already handling one.
type Notice struct {
	category   string
	args       Args
	suppressed bool
	reason     string
}

// New builds a notice of the given registered notice category.
//
// It does not validate eagerly — an unknown category, a missing lead, or a
// missing NextStep on a tone that requires one all surface from Payload or
// Publish, the one point where the message would otherwise go out wrong.
func New(category string, a Args) *Notice {
	return &Notice{category: category, args: a}
}

// Suppressed returns a notice that deliberately posts nothing, carrying the
// reason why.
//
// Staying silent is sometimes correct — a duplicate request already has a
// pending reply; a blocklisted requester must not learn the session exists.
// Saying so with a reason is what separates those cases from a message that
// went missing: a surface logs the reason, so "the user saw nothing" is always
// attributable to a decision someone made.
func Suppressed(reason string) *Notice {
	return &Notice{suppressed: true, reason: reason}
}

// IsSuppressed reports whether this notice should post nothing. A nil *Notice
// is suppressed: "no notice" and "post nothing" are the same instruction to a
// surface, and a nil check at every call site would be noise.
func (n *Notice) IsSuppressed() bool { return n == nil || n.suppressed }

// SuppressReason is why nothing is being posted. Callers log it; it is never
// shown to a user.
func (n *Notice) SuppressReason() string {
	if n == nil {
		return ""
	}
	return n.reason
}

// Category is the registry key this notice was built against. Empty for a
// suppressed or nil notice.
func (n *Notice) Category() string {
	if n == nil {
		return ""
	}
	return n.category
}

// Args returns the notice's copy. Renderers that need the raw fields (rather
// than a built payload) read it here.
func (n *Notice) Args() Args {
	if n == nil {
		return Args{}
	}
	return n.args
}

// Style resolves how this notice renders, from its registry row: tone, whether
// it is terminal, and any glyph override. The notice itself carries none of
// these, so a caller cannot override them.
func (n *Notice) Style() (channelinteractions.Tone, bool, channelinteractions.Glyph, error) {
	cat, err := n.category0()
	if err != nil {
		return "", false, "", err
	}
	return cat.Tone, cat.Terminal, cat.Glyph, nil
}

// category0 resolves and checks the registry row.
func (n *Notice) category0() (channelinteractions.Category, error) {
	if n == nil {
		return channelinteractions.Category{}, fmt.Errorf("notice: nil notice has no category")
	}
	if n.suppressed {
		return channelinteractions.Category{}, fmt.Errorf("notice: suppressed notice has no category (reason: %s)", n.reason)
	}
	cat, ok := channelinteractions.Get(n.category)
	if !ok {
		return channelinteractions.Category{}, fmt.Errorf("notice: category %q is not registered", n.category)
	}
	if !cat.Notice {
		return channelinteractions.Category{}, fmt.Errorf("notice: category %q is not a notice category (it declares a decision leg)", n.category)
	}
	return cat, nil
}

// Payload builds the wire payload for this notice. It is the single place the
// notice contract is enforced, so every surface and every publisher gets the
// same guarantees.
func (n *Notice) Payload(sess channelevents.SessionRef, requestRef string) (channelevents.InteractionRequestPayload, error) {
	var zero channelevents.InteractionRequestPayload
	cat, err := n.category0()
	if err != nil {
		return zero, err
	}
	if n.args.Lead == "" {
		return zero, fmt.Errorf("notice %q: lead must not be empty", n.category)
	}
	if cat.Tone.RequiresNextStep() && n.args.NextStep == "" {
		return zero, fmt.Errorf(
			"notice %q: tone %q requires a nextStep (a %s notice that names no action leaves the user stuck)",
			n.category, cat.Tone, cat.Tone)
	}
	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: sess,
		Category:        n.category,
		RequestRef:      requestRef,
		Lead:            n.args.Lead,
		Body:            n.args.Body,
		NextStep:        n.args.NextStep,
		Fields:          n.args.Fields,
		Excerpt:         n.args.Excerpt,
		Details:         n.args.Details,
		Audience:        n.args.Audience,
		// Actions is deliberately nil: a notice is read-only. Validate
		// permits nil Actions precisely for this shape.
	}
	if err := pl.Validate(); err != nil {
		return zero, fmt.Errorf("notice %q: %w", n.category, err)
	}
	return pl, nil
}

// Publish builds and publishes a notice on the session's OUT subject as a
// KindInteractionRequest envelope — the same wire path every prompt takes, so
// notices reach every surface the interaction model already reaches.
//
// A suppressed (or nil) notice publishes nothing and returns nil; the caller
// is expected to have logged SuppressReason.
//
// The envelope goes out UNSIGNED — this is the path for publishers with no
// identity key of their own (channelsd, webd, the authz hooks). A publisher
// that has one (the runner) calls PublishSigned instead.
func (n *Notice) Publish(
	pub channelevents.PublishFunc,
	sess channelevents.SessionRef,
	requestRef string,
) error {
	return n.PublishSigned(nil, pub, sess, requestRef)
}

// PublishSigned is Publish with the publisher's envelope signer: the envelope
// is signed with the session's identity key before it reaches the wire. A nil
// signer publishes unsigned — the same nil-signer delegation the
// channelevents publish helpers use — which is what lets Publish delegate
// here and every existing unsigned caller stay byte-identical.
func (n *Notice) PublishSigned(
	signer *channelevents.EnvelopeSigner,
	pub channelevents.PublishFunc,
	sess channelevents.SessionRef,
	requestRef string,
) error {
	if n.IsSuppressed() {
		return nil
	}
	if pub == nil {
		return fmt.Errorf("notice %q: no publish func wired", n.category)
	}
	pl, err := n.Payload(sess, requestRef)
	if err != nil {
		return err
	}
	if err := signer.PublishOut(pub, sess.Namespace, sess.Name,
		channelevents.KindInteractionRequest, pl); err != nil {
		return fmt.Errorf("notice %q: publish interaction_request: %w", n.category, err)
	}
	return nil
}
