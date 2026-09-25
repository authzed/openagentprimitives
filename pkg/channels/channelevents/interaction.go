// pkg/channels/channelevents/interaction.go
//
// The semantic Interaction model: ONE request/applied/decision envelope triple
// carries every prompt type (tool approval, info leakage, content inspection,
// identity choice, permission, provider retry, credentials, live-view offers,
// queued-message acks, metaagent scope). Channel kinds render the semantic
// payload and never see per-category logic; the categories themselves are
// registered in pkg/channels/channelinteractions.
package channelevents

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

const (
	// KindInteractionRequest is published (runner gates, channelsd
	// watchers, authzd) toward the channel: a prompt the user can act on.
	KindInteractionRequest Kind = "interaction_request"
	// KindInteractionApplied is published by the pipeline after a decision
	// (or timeout) resolves an interaction; surfaces render it as an
	// in-place edit of the original prompt.
	KindInteractionApplied Kind = "interaction_applied"
	// KindInteractionDecision is published by a surface (channel kind
	// listener, webchat route, view interact, hosted decision page) when
	// the user picks an action.
	KindInteractionDecision Kind = "interaction_decision"
)

// ActionKind says how a surface realizes an action.
type ActionKind string

const (
	ActionKindDecision ActionKind = "decision"  // button → InteractionDecision
	ActionKindLink     ActionKind = "link"      // static URL (signed deep-link, portal)
	ActionKindLinkMint ActionKind = "link_mint" // URL minted server-side on decision; returned via Applied.MintedURL
)

// ActionStyle is a rendering hint only; surfaces map it to their idiom.
type ActionStyle string

const (
	ActionStylePrimary ActionStyle = "primary"
	ActionStyleDanger  ActionStyle = "danger"
	ActionStyleDefault ActionStyle = ""
)

// Interaction outcomes. Categories interpret Approved/Denied; Resolved is
// for link-style interactions completed out-of-band (credential linked);
// Expired is a timeout (no decider).
const (
	OutcomeApproved = "approved"
	OutcomeDenied   = "denied"
	OutcomeExpired  = "expired"
	OutcomeResolved = "resolved"
)

// InteractionField is one label/value row ("Tool: git_push").
// Values are trusted publisher copy; never place untrusted content here.
type InteractionField struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Mentions optionally carries the FULL structured identity of the
	// party/parties this field names (e.g. info_leakage's "Would share with"
	// recipients) — the standing rule is to carry structured identity all the
	// way to the consumer that can act on it, never flatten to a display
	// string early. Value stays the always-present display fallback for
	// surfaces that don't resolve identities to mentions (builtin/local/fake
	// just render Value); a surface that CAN resolve identities to a native
	// mention (Slack: canonical → user id → "<@id>") should render Mentions
	// instead, falling back to that recipient's own display text on a
	// per-recipient resolution miss — a named party must never silently
	// disappear because a surface couldn't resolve it. Optional: nil for
	// every field that isn't about mentionable parties.
	Mentions []ExternalIdentity `json:"mentions,omitempty"`

	// Items is this field's value as STRUCTURE rather than prose, for surfaces
	// that can lay it out — a nested list with per-line emphasis.
	//
	// Same contract as Mentions, for the same reason: carry the structure all
	// the way to the consumer that can act on it, and never flatten early.
	// Value stays the always-present fallback, so a surface that renders no
	// structure (plain text, the TUI, a channel kind that has not adopted this)
	// is exactly as well off as before.
	//
	// The publisher owns both, and they must SAY THE SAME THING: Value is what
	// a reader sees when Items cannot be rendered, so a fact present in one and
	// absent from the other is a surface where the approval reads differently.
	Items []InteractionItem `json:"items,omitempty"`
}

// Tone is a rendering hint on one line of a structured field — emphasis, not
// meaning. A surface that ignores it must still be showing the truth, because
// the line's own text says what it is; the tone only decides how loudly.
type Tone string

const (
	// ToneReadonly marks a line whose effects never leave the call: nothing is
	// written anywhere. The lightest of the three blast-radius tiers.
	ToneReadonly Tone = "readonly"

	// ToneReadwrite marks a line that mutates state but keeps the mutation
	// inside the session's own scope — reversible, not reaching outward.
	ToneReadwrite Tone = "readwrite"

	// ToneExternal marks a line whose effects leave the session and cannot be
	// undone. It is the reason a card is worth reading, so it is the one thing
	// a surface should make impossible to skim past.
	ToneExternal Tone = "external"

	// ToneMuted marks supporting detail — present, deliberately quiet.
	ToneMuted Tone = "muted"
)

// InteractionItem is one line of a structured field, optionally with children.
//
// Deliberately generic: a plan-gate card renders phases with their permissions
// and resources, but nothing here is plan-gate-shaped, so any category with a
// list to show uses the same shape and every surface renders it one way.
type InteractionItem struct {
	// Text is the line itself. Publisher-authored and trusted, like Value —
	// untrusted content belongs in InteractionExcerpt, which is rendered inert.
	Text string `json:"text"`

	// Detail is the secondary half of the line — the concrete instance a
	// permission acts on, typically. Kept separate from Text so a surface can
	// give it its own weight instead of parsing an em-dash out of a sentence.
	Detail string `json:"detail,omitempty"`

	// Tone is the emphasis hint. Empty renders as ordinary.
	Tone Tone `json:"tone,omitempty"`

	// Hint is supplementary text a surface MAY reveal on demand — a tooltip,
	// a title attribute — never rendered inline as part of the line itself. A
	// surface that has no on-demand affordance (plain text, a channel message)
	// must simply omit it; concatenating it into Text or Detail defeats the
	// reason it is a separate field.
	Hint string `json:"hint,omitempty"`

	// Items are this line's children, one level of nesting being all any
	// current category needs.
	Items []InteractionItem `json:"items,omitempty"`

	// Icon names a glyph from a CLOSED, surface-defined registry — never a
	// URL. Publisher-authored: the plan gate sets it from a resource type's
	// declared display (SpiceDBResourceDisplay.Icon), never from anything an
	// agent wrote. A surface that does not recognize the name must render no
	// icon at all — never a fallback image, never a guess.
	Icon string `json:"icon,omitempty"`

	// Href, when set, makes Detail a real link: a surface renders Detail's own
	// text as the anchor, targeting Href. The publisher MUST set Href to the
	// exact same string as Detail — never a different one — so text == href
	// holds by construction. This is the whole defence against a link that
	// says one thing and goes somewhere else: nothing upstream of this field
	// ever transforms the value, it either qualifies unchanged or is left
	// empty (see resourcedisplay.EligibleHref). https only. Empty means Detail is
	// plain text.
	Href string `json:"href,omitempty"`
}

// InteractionExcerpt is UNTRUSTED content shown for human judgment
// (content-inspection previews). CONTRACT: every field of the struct (Label and
// Content alike) must be rendered inert — fenced/quoted, never interpreted as
// markup, never adjacent to actions in a way that lets any field impersonate
// them. No field may ever be emitted outside the inert treatment.
type InteractionExcerpt struct {
	Label   string `json:"label,omitempty"`
	Content string `json:"content"`
}

// InteractionResourceRef names a source resource whose #owner may decide a
// DecideResourceOwners interaction. Mirrors authz.ApproverResourceRef by shape;
// kept an interaction-model type so the generic wire contract is self-contained
// (the channel model never imports authz). Uses Type (SpiceDB nomenclature).
type InteractionResourceRef struct {
	// Type is the SpiceDB object type ("repo", "data", …) and ID its object id;
	// together they name the object whose Permission holders may decide.
	Type string `json:"type"`
	ID   string `json:"id"`

	// Permission is the relation or permission an approver must hold on this
	// object for their click to count — the resource type's declared
	// approverPermission.
	//
	// On the wire because CLICK-time enforcement happens in channelsd, in a
	// different process from the runner that raised the ask. channelsd used to
	// rebuild the subject-set as "<type>:<id>#owner", hardcoding an assumption
	// the runner no longer makes; carrying the answer here is what keeps the two
	// halves from disagreeing about who may approve.
	//
	// Empty means the publisher named no per-resource pool (a session-only type),
	// and the session approve-set is the gate.
	Permission string `json:"permission,omitempty"`
}

// InteractionAction is one thing the user can do with the prompt.
type InteractionAction struct {
	// ID is what a decision carries back as ActionID — the category's handler
	// switches on it. Required, and unique within one prompt.
	ID string `json:"id"`
	// Label is the button text. Required.
	Label string `json:"label"`
	// Style is a rendering hint; empty renders as the surface's default.
	Style ActionStyle `json:"style,omitempty"`
	// Kind says how the surface realizes the action. Required.
	Kind ActionKind `json:"kind"`
	URL  string     `json:"url,omitempty"` // ActionKindLink only
}

// InteractionAudience says who may see and act on the prompt.
type InteractionAudience struct {
	// Scope accepts AudienceRequester, AudienceApprovers, or AudienceParticipants
	// — a deliberate 3-value subset of the 4-value Audience enum, enforced by
	// Validate(). AudienceParticipants is the addressee-less broadcast case: the
	// prompt is posted to the session channel/thread for every interact-participant
	// to see (e.g. provider_error_retry's no-addressee Retry button), so it
	// carries no Requester/Approvers identity — decision authz for who may
	// action it is re-checked server-side via the category's DecideParticipant
	// policy, not by this audience list. AudienceSpecificUser is not yet wired
	// through this generic model (still legacy-only, see authzclass.go).
	Scope Audience `json:"scope"`
	// Requester identifies the addressee for AudienceRequester scope. The
	// decision pipe uses it for the DecideRequester standing check
	// (canonical-identity equality, fail-closed when absent). Required when
	// Scope == AudienceRequester.
	Requester *ExternalIdentity `json:"requester,omitempty"`
	// Approvers is populated by the publisher for AudienceApprovers scope
	// (resolved via the owner-derived approver model). Decision authz is
	// re-checked server-side; this list is for delivery routing only.
	Approvers []ExternalIdentity `json:"approvers,omitempty"`
	// PublicNote asks the surface to additionally post a non-actionable
	// "approval pending" note visible to the whole conversation (Slack:
	// in-thread note alongside the private prompt).
	PublicNote bool `json:"publicNote,omitempty"`
	// PublicNoteBody is the exact inert text a surface posts as the public
	// "approval pending" note when PublicNote is true. Publisher-authored;
	// rendered inert (never interpreted as markup adjacent to actions). Empty
	// ⇒ the surface posts a generic "approval pending" note.
	PublicNoteBody string `json:"publicNoteBody,omitempty"`
}

// InteractionRequestPayload is the body of a KindInteractionRequest envelope.
//
// CONTRACT: every field EXCEPT Excerpt is publisher-authored and trusted —
// surfaces render Lead/Body/Fields/Action labels as live markup. Untrusted or
// externally-derived content (tool output, inspected data, third-party strings)
// MUST be routed through Excerpt, never through the trusted fields.
type InteractionRequestPayload struct {
	// AgentSessionRef is a denormalized copy for renderer convenience, not
	// validated here. It is NOT a routing input, and neither is the envelope's
	// Session field: on the bus the NATS subject is the routing authority and
	// Envelope.Session is only cross-checked against it (see Envelope.Session).
	// Same for Applied and Decision payloads.
	AgentSessionRef SessionRef `json:"agentSessionRef"`
	// AskingChain is the delegation path from the asking session up to the
	// session whose channel this card is being delivered through: asker first,
	// that ancestor last. len-1 is the number of hops between them.
	//
	// PLATFORM-AUTHORED, unlike everything else in this payload. The outbound
	// relay stamps it from the lineage walk it already performs to route a
	// human-directed envelope, and OVERWRITES whatever a publisher put here.
	// That is the point: a chain an asking agent could write would let it claim
	// to be a child of a session the approver trusts — the same forgery the
	// AgentSessionRef correction exists to stop, one level up.
	//
	// Empty on a card delivered through the asking session's own binding
	// (there is no relationship to explain) and on any path that did not go
	// through the lineage walk.
	// +optional
	AskingChain []SessionRef `json:"askingChain,omitempty"`
	// Category is the channelinteractions registry key; it decides the
	// decision policy, tone, and category-aware labelling. Required.
	Category string `json:"category"`
	// RequestRef is the publisher-minted id every later envelope about this
	// prompt (applied, decision, rejected) carries back. Required.
	RequestRef string `json:"requestRef"`
	// Lead is the one-sentence headline. Required.
	Lead string `json:"lead"`
	// Body is optional supporting detail below the lead.
	Body string `json:"body,omitempty"`
	// NextStep is what the user should DO about this — one imperative
	// sentence ("Start a new thread to try again."), rendered on its own
	// line and emphasised by every surface.
	//
	// It is a field rather than a sentence inside Body so that "we left the
	// user stuck" is a structural property a test can assert, not a review
	// miss. The house-style conformance test requires it for every notice
	// category whose tone says so; see channelinteractions.Tone.RequiresNextStep.
	NextStep string `json:"nextStep,omitempty"`
	// Fields are trusted label/value supporting rows; Excerpt is the ONLY
	// place untrusted content may travel (see the type's contract).
	Fields  []InteractionField  `json:"fields,omitempty"`
	Excerpt *InteractionExcerpt `json:"excerpt,omitempty"`
	// Actions is what the user can do; nil is legal (a read-only notice card).
	Actions []InteractionAction `json:"actions,omitempty"`
	// Details is the category's opaque side-band payload (tool_approval puts
	// ToolApprovalDetails here). Never rendered as copy by a surface.
	Details json.RawMessage `json:"details,omitempty"`
	// Resources names the source resources whose #owner may decide a
	// DecideResourceOwners interaction (tool_call → resource#owner, info_leakage
	// → data#owner). Empty ⇒ the session approve-set decides. Persisted onto the
	// durable memapproval record at publish time so the decision pipe can recover
	// it after a channelsd restart.
	Resources []InteractionResourceRef `json:"resources,omitempty"`
	// Audience says who may see and act on the prompt. Validated.
	Audience InteractionAudience `json:"audience"`
	// ExpiresAt is when the prompt lapses unanswered (outcome "expired"); nil
	// means it waits indefinitely.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Interruptible marks a prompt a mid-turn interrupt may cut short, so a
	// surface can offer "Interrupt & Send Now" alongside the actions.
	Interruptible bool `json:"interruptible,omitempty"`
}

// Validate enforces the wire contract. Nil Actions is legal: a read-only
// notice card (info-leakage notice-only mode).
func (p InteractionRequestPayload) Validate() error {
	if p.Category == "" {
		return fmt.Errorf("interaction request: category must not be empty")
	}
	if p.RequestRef == "" {
		return fmt.Errorf("interaction request: requestRef must not be empty")
	}
	if p.Lead == "" {
		return fmt.Errorf("interaction request: lead must not be empty")
	}
	seen := map[string]bool{}
	for i, a := range p.Actions {
		if a.ID == "" || a.Label == "" {
			return fmt.Errorf("interaction request: action[%d] needs both id and label", i)
		}
		if seen[a.ID] {
			return fmt.Errorf("interaction request: duplicate action id %q", a.ID)
		}
		seen[a.ID] = true
		switch a.Kind {
		case ActionKindDecision, ActionKindLinkMint:
			if a.URL != "" {
				return fmt.Errorf("interaction request: action %q kind %q must not carry a url", a.ID, a.Kind)
			}
		case ActionKindLink:
			if a.URL == "" {
				return fmt.Errorf("interaction request: link action %q needs a url", a.ID)
			}
			u, err := url.Parse(a.URL)
			if err != nil {
				return fmt.Errorf("interaction request: link action %q has invalid url: %v", a.ID, err)
			}
			if u.Scheme != "https" && u.Scheme != "http" {
				return fmt.Errorf("interaction request: link action %q url scheme must be http or https, got %q", a.ID, u.Scheme)
			}
		default:
			return fmt.Errorf("interaction request: action %q has unknown action kind %q", a.ID, a.Kind)
		}
	}
	switch p.Audience.Scope {
	case AudienceRequester:
		// A requester identity is either the natural raw+email form
		// (Kind+ExternalID) or, when only a resolved canonical is available
		// (e.g. a started-by user with no raw+email annotations on record),
		// the Subject passthrough — see ExternalIdentity.Principal(), which
		// treats Subject as authoritative when set.
		hasRaw := p.Audience.Requester != nil && p.Audience.Requester.Kind != "" && p.Audience.Requester.ExternalID != ""
		hasSubject := p.Audience.Requester != nil && p.Audience.Requester.Subject != ""
		if !hasRaw && !hasSubject {
			return fmt.Errorf("interaction request: requester scope needs the requester identity")
		}
	case AudienceApprovers:
		if len(p.Audience.Approvers) == 0 {
			return fmt.Errorf("interaction request: approvers scope needs at least one approver")
		}
		// At least one approver must be ADDRESSABLE, by the same rule the
		// requester scope uses above. A non-empty slice holding an identity
		// with no raw (Kind+ExternalID) and no Subject passes a bare len()
		// check but gives the sender nobody to deliver to: the request is
		// parked durably, the requester waits forever, and nothing errors.
		//
		// A cron-spawned session hits exactly this — it has no started-by
		// annotations, and the join-approval path builds its approver from
		// them — so a human replying in a cron thread silently produced an
		// unapprovable request. Fail closed here so it surfaces as an error
		// the caller must handle rather than as silence.
		addressable := false
		for _, a := range p.Audience.Approvers {
			if (a.Kind != "" && a.ExternalID != "") || a.Subject != "" {
				addressable = true
				break
			}
		}
		if !addressable {
			return fmt.Errorf("interaction request: approvers scope needs at least one addressable approver (raw kind+externalId, or a subject passthrough); got %d approver(s) with no addressable identity", len(p.Audience.Approvers))
		}
	case AudienceParticipants:
		// Addressee-less broadcast: no Requester/Approvers identity required —
		// every interact-participant is a valid viewer, and decision authz is
		// re-checked server-side via the category's DecideParticipant policy.
	default:
		return fmt.Errorf("interaction request: unknown audience scope %q", p.Audience.Scope)
	}
	return nil
}

// InteractionAppliedPayload is the body of a KindInteractionApplied envelope.
type InteractionAppliedPayload struct {
	AgentSessionRef SessionRef `json:"agentSessionRef"`
	// Category and RequestRef identify the prompt this resolves; both required.
	Category   string `json:"category"`
	RequestRef string `json:"requestRef"`
	// Outcome is one of the Outcome* constants. Required, and validated.
	Outcome string `json:"outcome"`
	// OutcomeText is the category's own answer beyond approved/denied — the
	// chosen action id for a multi-way prompt. Empty when the Outcome says
	// everything (both tool-approval handlers resolve with no OutcomeText).
	OutcomeText string            `json:"outcomeText,omitempty"`
	DecidedBy   *ExternalIdentity `json:"decidedBy,omitempty"` // nil for timeouts / out-of-band resolution
	// Reason is optional human-readable colour on the outcome ("timeout").
	Reason    string `json:"reason,omitempty"`
	MintedURL string `json:"mintedUrl,omitempty"` // ActionKindLinkMint result
	// ResponseRef is the surface-opaque handle for editing the clicker's
	// artifact (Slack: response_url). Round-tripped from the decision.
	ResponseRef string `json:"responseRef,omitempty"`
}

func (p InteractionAppliedPayload) Validate() error {
	if p.Category == "" {
		return fmt.Errorf("interaction applied: category must not be empty")
	}
	if p.RequestRef == "" {
		return fmt.Errorf("interaction applied: requestRef must not be empty")
	}
	switch p.Outcome {
	case OutcomeApproved, OutcomeDenied, OutcomeExpired, OutcomeResolved:
		return nil
	case "":
		return fmt.Errorf("interaction applied: outcome must not be empty")
	default:
		return fmt.Errorf("interaction applied: unknown outcome %q", p.Outcome)
	}
}

// InteractionDecisionPayload is the body of a KindInteractionDecision envelope.
type InteractionDecisionPayload struct {
	AgentSessionRef SessionRef `json:"agentSessionRef"`
	// Category and RequestRef identify the prompt clicked; both required.
	Category   string `json:"category"`
	RequestRef string `json:"requestRef"`
	// ActionID is the InteractionAction.ID the user chose. Required.
	ActionID string `json:"actionId"`
	// Decider is the clicker as the surface saw them — a CLAIM. The decision
	// pipe re-derives the canonical subject and re-checks standing; never an
	// authorization input on its own. Validate requires kind + externalId.
	Decider ExternalIdentity `json:"decider"`
	// ResponseRef is the surface-opaque handle for editing this clicker's own
	// artifact (Slack: response_url); round-tripped onto the applied echo.
	ResponseRef string `json:"responseRef,omitempty"`
}

func (p InteractionDecisionPayload) Validate() error {
	if p.Category == "" {
		return fmt.Errorf("interaction decision: category must not be empty")
	}
	if p.RequestRef == "" {
		return fmt.Errorf("interaction decision: requestRef must not be empty")
	}
	if p.ActionID == "" {
		return fmt.Errorf("interaction decision: actionId must not be empty")
	}
	if p.Decider.Kind == "" || p.Decider.ExternalID == "" {
		return fmt.Errorf("interaction decision: decider needs kind and externalId")
	}
	return nil
}
