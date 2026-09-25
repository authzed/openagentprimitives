// pkg/channels/channelevents/metaagent.go
//
// The metaagent family's wire contract: the four subjects, the payload shapes
// that had no type of their own, and the publish/decode helpers every producer
// and consumer goes through.
//
// # Why these four are Kinds
//
// Naming them keeps the subject grammar in one place and routes both
// directions through the same parser as every other kind, so their strictness
// (a validated in/out segment token, a refused empty ns/name) cannot drift.
//
// # Wire format: bare today, tolerant readers first
//
// These four travel as BARE JSON payloads, not wrapped in an Envelope, and
// PublishMetaagentIn / PublishMetaagentOut publish them that way on purpose.
// Wrapping them is a wire change whose two sides roll independently: an authzd
// publishing an Envelope to a not-yet-rolled channelsd would have the whole
// envelope unmarshalled INTO scope.MetaagentApprovalPayload, yielding an
// approval card with an empty requestId — buttons that resolve nothing, while
// the runner blocks the entire approval window and then halts. A
// silently-dropped in-flight approval is far worse than log noise.
//
// So readers go first. DecodeMetaagentIn / DecodeMetaagentOut accept BOTH
// shapes and, when the body IS an envelope, enforce the same subject-vs-session
// cross-check the relay and channelsd's inbound handlers enforce. Once every
// consumer in a cluster is a tolerant reader, flipping the publishers is a
// one-line change here with no flag day.
//
// # Payload types
//
// Two of the four get their type here. The other two are declared elsewhere,
// each where its own single source of truth belongs, and are named here rather
// than re-declared — a second spelling of a wire contract is worse than a doc
// reference:
//
//   - metaagent_scope_approval → pkg/authz/scope.MetaagentApprovalPayload,
//     next to the ScopeDelta / SkippedItem / CaveatItem it carries and to the
//     hook that composes it. This package deliberately stays free of
//     authz/scope — see MetaagentRequestPayload.Envelope, which stays a
//     json.RawMessage for the same reason — so the type is not referenced in
//     code from here.
//   - metaagent_notice → pkg/channels/channelkinds.MetaagentNoticePayload,
//     next to the sub-channel senders that render it and to
//     ResolveNoticeRecipient, the addressing rule that is a property of that
//     payload. channelkinds imports channelevents and never the reverse, so it
//     cannot be referenced from here at all.
package channelevents

import (
	"encoding/json"
	"errors"
	"fmt"
)

// isMetaagent reports whether k is one of the four kinds in this file. It is
// the single enumeration of the family: Kind.RelayHandles is its complement
// (the relay consumes every kind EXCEPT these), and publishMetaagent gates on
// it so the bare wire format cannot leak onto a kind whose consumers expect an
// Envelope. Adding a fifth metaagent kind means adding a case here and nowhere
// else.
func (k Kind) isMetaagent() bool {
	switch k {
	case KindMetaagentRequest, KindMetaagentScopeApproval,
		KindMetaagentApprovalApplied, KindMetaagentNotice:
		return true
	default:
		return false
	}
}

// MetaagentRequestPayload is the body of ap.session.<ns>.<name>.in.metaagent_request.
//
// Two publishers: a channel kind's listener, when a user @-mentions the
// metaagent bot in a live session's thread (mid-session), and the runner's
// cold-start scope hook on a new session's first turn (ColdStart true).
//
// Everything here is PUBLISHER-SUPPLIED and the subject is inside the runner's
// own NATS grant, so authzd treats the whole body as untrusted input. In
// particular there is deliberately no autoApply field: whether the human
// approval gate is waived is resolved from the session's own
// authz_session_config snapshot, never from a field the requester chooses.
type MetaagentRequestPayload struct {
	// Requester is the canonical SpiceDB subject ("user:<canonical>") of the
	// user asking for the scope change. Channel kinds canonicalize before
	// publishing: authzd's mid-session gate checks
	// agentsession#manage_scope@user:<canonical>, which a raw channel-native id
	// would never match, denying every legitimate owner.
	Requester string `json:"requester"`
	// Text is the request in the user's own words, with the bot mention
	// stripped.
	Text string `json:"text"`
	// ColdStart routes the request to the new-session first-turn branch of the
	// staged lifecycle instead of the mid-session one.
	ColdStart bool `json:"coldStart,omitempty"`
	// Ambient marks the third trigger: classified because the session runs
	// with metaagent.trigger shadow or inline, not because anyone addressed
	// the metaagent.
	Ambient bool `json:"ambient,omitempty"`
	// Shadow means run the whole classify-and-decide path and apply NOTHING —
	// paired with Ambient to measure what an inline/shadow trigger would have
	// done before it is allowed to mutate scope.
	Shadow bool `json:"shadow,omitempty"`
	// Envelope is the static AgentClass envelope (bound entities + tools) the
	// runner assembled, so classification can keep in-envelope changes instead
	// of dropping them as out-of-envelope. Typed as raw JSON here to keep
	// channelevents free of the authz/scope package; authzd decodes it into
	// scope.AgentClassEnvelope.
	Envelope json.RawMessage `json:"envelope,omitempty"`
	// ApprovalTimeout is the class's configured authz.approvalTimeout as a Go
	// duration string. Empty means "not specified" — the executor falls back to
	// its own default.
	ApprovalTimeout string `json:"approvalTimeout,omitempty"`
}

// MetaagentApprovalAppliedPayload is the body of
// ap.session.<ns>.<name>.in.metaagent_approval_applied: one human's click on a
// metaagent scope-approval prompt, published by the channel kind that rendered
// it.
//
// It carries NO session reference, and that is the design: the session is the
// one the subject authorized. ApproverCanonical is a claim the publisher makes
// about the clicker, and authzd re-checks it against agentsession#manage_scope
// before the decision moves anything.
type MetaagentApprovalAppliedPayload struct {
	// RequestID matches the requestId of the scope-approval prompt clicked.
	RequestID string `json:"requestId"`
	// Approved is true for every "proceed" variant, false for deny.
	Approved bool `json:"approved"`
	// ApproverID is the clicker's channel-native user id, kept for display and
	// for addressing them with a follow-up notice.
	ApproverID string `json:"approverId"`
	// ApproverCanonical is the clicker's canonical SpiceDB subject
	// ("user:<canonical>"), resolved by the channel kind because authzd has no
	// channel API to canonicalize a bare user id. It is a claim, gated on
	// arrival — never an authorization by itself.
	ApproverCanonical string `json:"approverCanonical"`
	// Action disambiguates the cold-start variants (approve_cleaned /
	// approve_original / run_without_scope / deny); the mid-session prompt
	// carries it harmlessly.
	Action string `json:"action"`
}

// Errors returned by DecodeMetaagentIn / DecodeMetaagentOut. Each is a distinct
// sentinel so a caller can log the failure in the terms its own users would
// recognize rather than re-deriving the reason from a string.
var (
	// ErrMetaagentSubject means the subject is not a well-formed
	// ap.session.<ns>.<name>.<in|out>.<kind> for the requested direction, or
	// carries an empty ns/name token.
	ErrMetaagentSubject = errors.New("channelevents: not a well-formed metaagent subject")
	// ErrMetaagentKind means the subject's leaf is not a metaagent kind, or is
	// not the one the caller expected.
	ErrMetaagentKind = errors.New("channelevents: subject is not the expected metaagent kind")
	// ErrMetaagentSessionMismatch means the body was envelope-shaped and
	// claimed a session other than the one the subject authorized. This is the
	// impersonation case: drop, and log loudly.
	ErrMetaagentSessionMismatch = errors.New("channelevents: envelope session does not match the authorized subject")
	// ErrMetaagentPayload means the body is neither valid JSON nor a decodable
	// envelope.
	ErrMetaagentPayload = errors.New("channelevents: malformed metaagent payload")
)

// MetaagentMessage is one decoded metaagent message: the session the SUBJECT
// authorized, the kind the subject addresses, and the payload bytes the
// consumer should unmarshal into its own type.
type MetaagentMessage struct {
	// Namespace and Name come from the subject, always — never from the body.
	Namespace string
	Name      string
	// Kind is the subject's leaf, already checked against the caller's
	// expectation.
	Kind Kind
	// Payload is the metaagent payload proper: the body itself for a bare
	// publish, or the Envelope's payload field for an enveloped one.
	Payload json.RawMessage
	// Enveloped reports which shape arrived. Diagnostic — it is what makes the
	// wire-format rollout observable in logs — and nothing routes on it.
	Enveloped bool
}

// SessionRef is the "<ns>/<name>" form used in logs and memory scope IDs.
func (m MetaagentMessage) SessionRef() string { return m.Namespace + "/" + m.Name }

// DecodeMetaagentIn decodes one raw NATS message received on an IN metaagent
// subject, requiring the subject's leaf to be want.
//
// DecodeMetaagentOut is its outbound twin; both are thin wrappers over the same
// body so the two directions cannot differ in strictness, exactly as
// ParseInSubject / ParseOutSubject cannot.
func DecodeMetaagentIn(subject string, data []byte, want Kind) (MetaagentMessage, error) {
	ns, name, k, ok := ParseInSubjectKind(subject)
	return decodeMetaagent(subject, data, want, ns, name, k, ok)
}

// DecodeMetaagentOut decodes one raw NATS message received on an OUT metaagent
// subject, requiring the subject's leaf to be want.
func DecodeMetaagentOut(subject string, data []byte, want Kind) (MetaagentMessage, error) {
	ns, name, k, ok := ParseOutSubjectKind(subject)
	return decodeMetaagent(subject, data, want, ns, name, k, ok)
}

// metaagentEnvelope is the shape probe for "is this body an Envelope?".
//
// It is a struct rather than a reuse of Envelope so the probe cannot start
// depending on fields Envelope grows later. The discriminator is all three of
// v==1, a non-empty kind, and a payload — no metaagent payload has ever
// carried any of the three, let alone all of them, so a bare body can never be
// mistaken for an envelope and vice versa.
type metaagentEnvelope struct {
	Version int             `json:"v"`
	Kind    Kind            `json:"kind"`
	Session SessionRef      `json:"session"`
	Payload json.RawMessage `json:"payload"`
}

func decodeMetaagent(subject string, data []byte, want Kind, ns, name string, k Kind, subjectOK bool) (MetaagentMessage, error) {
	if !subjectOK {
		return MetaagentMessage{}, fmt.Errorf("%w: %q", ErrMetaagentSubject, subject)
	}
	if k != want {
		return MetaagentMessage{}, fmt.Errorf("%w: got %q, want %q", ErrMetaagentKind, k, want)
	}
	msg := MetaagentMessage{Namespace: ns, Name: name, Kind: k, Payload: json.RawMessage(data)}

	// One decode answers both questions: is this well-formed JSON at all, and is
	// it envelope-shaped? A body that is neither — a scalar, an array, garbage —
	// fails here, naming the subject, rather than surfacing later as a parse
	// error from inside a channel kind's Sender.
	var probe metaagentEnvelope
	if err := json.Unmarshal(data, &probe); err != nil {
		return MetaagentMessage{}, fmt.Errorf("%w: %v", ErrMetaagentPayload, err)
	}
	if probe.Version != 1 || probe.Kind == "" || len(probe.Payload) == 0 {
		// A bare payload: hand the body through to the consumer untouched.
		return msg, nil
	}

	// Envelope-shaped. The subject is still the routing authority; the claim in
	// the body only ever gets to disagree, never to decide.
	if probe.Kind != k {
		return MetaagentMessage{}, fmt.Errorf("%w: envelope kind %q on a %q subject", ErrMetaagentKind, probe.Kind, k)
	}
	if probe.Session.Namespace != ns || probe.Session.Name != name {
		return MetaagentMessage{}, fmt.Errorf("%w: subject %s/%s, claimed %s/%s",
			ErrMetaagentSessionMismatch, ns, name, probe.Session.Namespace, probe.Session.Name)
	}
	msg.Payload = probe.Payload
	msg.Enveloped = true
	return msg, nil
}

// PublishMetaagentIn publishes payload on SubjectIn(SubjectPrefix(ns, name), k)
// for a metaagent kind. PublishMetaagentOut is its outbound twin.
//
// Unlike PublishIn / PublishOut these publish the payload BARE — no Envelope
// wrapper — because the readers are rolling out first; see the package comment.
// Going through here rather than a local fmt.Sprintf is still what makes the
// subject grammar single-sourced and the kind a closed enum at the publish
// boundary.
func PublishMetaagentIn(publish PublishFunc, ns, name string, k Kind, payload any) error {
	return publishMetaagent(publish, SubjectIn(SubjectPrefix(ns, name), k), ns, name, k, payload)
}

// PublishMetaagentOut publishes payload on SubjectOut(SubjectPrefix(ns, name), k)
// for a metaagent kind.
func PublishMetaagentOut(publish PublishFunc, ns, name string, k Kind, payload any) error {
	return publishMetaagent(publish, SubjectOut(SubjectPrefix(ns, name), k), ns, name, k, payload)
}

func publishMetaagent(publish PublishFunc, subject, ns, name string, k Kind, payload any) error {
	if publish == nil {
		return fmt.Errorf("channelevents: publish of %q: nil publisher (wiring bug)", k)
	}
	if !k.isMetaagent() {
		// Every other kind's consumers expect an Envelope; publishing one of
		// them bare through here would hand a decoder a payload with no version,
		// no kind and no session.
		return fmt.Errorf("channelevents: %q is not a metaagent kind; use PublishIn/PublishOut", k)
	}
	if ns == "" || name == "" {
		return fmt.Errorf("channelevents: publish of %q: empty session (ns=%q name=%q)", k, ns, name)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("channelevents: marshal %q payload: %w", k, err)
	}
	return publish(subject, body)
}
