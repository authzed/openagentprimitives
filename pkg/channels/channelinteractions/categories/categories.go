// Package categories holds the production interaction-category rows. Importing
// it (blank import in each binary that renders or decides interactions)
// registers the rows into the process-wide channelinteractions registry.
// Rows are declarative; decision handlers are Bound separately in the binary
// that runs the pipeline (see internal/cmd/channelsd/main.go).
package categories

import (
	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// Category name constants — the single source of truth shared by publishers,
// renderers, and Bind sites.
const (
	CredentialLink = "credential_link"
	// CredentialUpdate REPLACES a credential the platform independently
	// re-verified is dead (a live probe rejected it, or a refresh came back
	// invalid_grant); CredentialLink is the FIRST-TIME connection prompt for a
	// credential never linked at all. Two rows, not one, because they are
	// raised by different producers for different reasons and their cards carry
	// different content — they share only the AwaitingCredentials park.
	CredentialUpdate = "credential_update"
	// WorkshopCredential asks the builder session's requester to connect a
	// credential for a DIFFERENT identity — one belonging to the workshop bot
	// under construction, not to the builder session itself. Unlike
	// CredentialLink/CredentialUpdate it must NOT park the builder: the
	// person connects the workshop bot's credential asynchronously while the
	// builder keeps authoring, so it carries no Park phase at all (see
	// PortalAccess for the same non-parking shape).
	WorkshopCredential = "workshop_credential"
	IdentityChoice     = "identity_choice"
	PortalAccess       = "portal_access"
	PermissionRequest  = "permission_request"
	// StartApproval asks a PLATFORM ADMIN whether an org non-member (a
	// channel guest) may start the parked session at all. Its own row rather
	// than a reuse of PermissionRequest — that card's approver is the
	// session's owner/started_by seat, which a parked guest session does not
	// have yet, and its grant is `interact` on a running session rather than
	// permission to exist.
	StartApproval      = "start_approval"
	ProviderErrorRetry = "provider_error_retry"
	QueuedMessages     = "queued_messages"
	ContentInspection  = "content_inspection"
	// ToolApproval and InfoLeakage are the two resource-owner-gated approval
	// categories. Their grant handlers are Bound in internal/cmd/channelsd:
	// BindToolApprovalHandler side-effects a grant write on approve;
	// info_leakage binds the pure ApprovalDecisionHandler, since its grant is
	// written runner-side via the interaction_applied bridge.
	ToolApproval = "tool_approval"
	InfoLeakage  = "info_leakage"
	// DataSlotDisclosure asks whether one agent may hand a specific datum to
	// ANOTHER AGENT it is delegating to.
	//
	// Its own row rather than a reuse of InfoLeakage, which asks the adjacent
	// question "may this datum go to these PEOPLE". The counterparty differs,
	// and so does what a decider needs to see — the child class receiving it,
	// and the slot it fills — so the two cards are not interchangeable even
	// though both gate a disclosure.
	//
	// The grant is written OPERATOR-side (the SubagentRequest controller binds
	// on its next pass after channelsd records the approval on status), unlike
	// tool_approval's channelsd-side write or info_leakage's runner-side one.
	DataSlotDisclosure = "data_slot_disclosure"
	// InfoLeakageNotice is the read-only sibling of InfoLeakage: in "logging"
	// mode the audience gate never blocks, so instead of pausing for an owner's
	// approval it delivers a zero-action notice to the requester.
	InfoLeakageNotice = "info_leakage_notice"

	// PlanPhase and PlanAmendment are the plan gate's two approval categories.
	//
	// PlanPhase clears a declared phase to RUN — the feature's namesake, and the
	// point at which an agent's self-declared ceiling stops being self-approving.
	// PlanAmendment is a justified retry: reach the approved plan does not hold.
	//
	// Both gate on the SESSION approve-set rather than a per-resource owner
	// intersection: a plan ceiling names KINDS of action, not a specific external
	// resource, so there is no resource counterparty to route consent to.
	PlanPhase     = "plan_phase"
	PlanAmendment = "plan_amendment"

	// PreconditionWaiver is the human WAIVER card for a slot whose precondition
	// — a CEL predicate over signed facts — Refused. Approving it binds the slot
	// grant anyway, which is BY DESIGN the waiver: this card's handler calls
	// authz.BindApproved with PreconditionsWaived, the one policy that skips the
	// precondition filter, because the whole answer to a Refused verdict is that
	// a person may consent to it from a card that explains what they consent to.
	//
	// It is its OWN category, never a reused tool_approval card: reusing
	// tool_approval would conflate two distinct consents, since approving a
	// tool_approval card also writes a slot binding. This constant is the ONE
	// shared symbol — the raise-side ask Kind, this Category Name, and the
	// PublishApproval switch case must all be THIS constant, not a re-typed
	// literal, or a drift becomes a fail-closed "unknown approval kind" at raise
	// time that still compiles.
	PreconditionWaiver = "precondition_waiver"
	// UserPreferenceConfirm asks the turn author (the addressee) to confirm
	// saving a user preference. The session is parked until the addressee decides.
	UserPreferenceConfirm = "user_preference_confirm"
	GoalExecutionConsent  = "goal_execution_consent"
)

// registerPrompts registers every prompt category. Kept out of init for the
// same reason as registerNotices: a test that Resets the registry can put it
// back, which an init cannot do.
func registerPrompts() {
	channelinteractions.Register(channelinteractions.Category{Name: GoalExecutionConsent, PlanConsent: true, Tone: channelinteractions.TonePrivacy, Deciders: channelinteractions.DecideRequester, Resurface: channelinteractions.ResurfaceCached, Surface: channelinteractions.SurfaceDMOnly})
	channelinteractions.Register(channelinteractions.Category{
		Name: PlanPhase, IncludesConsents: true,
		Park: v1alpha1.AgentSessionPhaseAwaitingDecision,
		// Routine, like tool_approval and for the same reason: the agent is
		// asking to do something ordinary, and a plan gate fires on the common
		// path rather than the alarming one. Reaching for a louder tone here
		// would spend the strong signals on the most frequent prompt in the
		// system, which is what makes them stop meaning anything.
		Tone: channelinteractions.ToneRoutine,
		// The runner blocks the gated call on this decision; without a resume
		// policy the applied envelope is dropped and the session waits out its
		// full approval timeout.
		Resume: channelinteractions.ResumeApproval,
		// Same session-approve-set gating as content_inspection, for the same
		// reason: a phase ceiling names kinds of action, not one external
		// resource, so there is no owner intersection to compute.
		Deciders: channelinteractions.DecideApprovers,
		// The card is fixed for the life of the request (a frozen ceiling plus a
		// computed summary), so a re-interacting approver gets it republished
		// verbatim rather than a fresh publisher round-trip.
		Resurface: channelinteractions.ResurfaceCached,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      PlanAmendment,
		Park:      v1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:      channelinteractions.ToneRoutine,
		Resume:    channelinteractions.ResumeApproval,
		Deciders:  channelinteractions.DecideApprovers,
		Resurface: channelinteractions.ResurfaceCached,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name: PreconditionWaiver,
		// ToneCritical, not tool_approval's ToneRoutine: a waiver is raised ONLY
		// after a slot precondition Refused, and approving it overrides a
		// fact-gated refusal — one of the strongest consents in the system. It
		// shares ToneCritical with content_inspection; info_leakage carries a
		// comparably strong consent but under TonePrivacy. Either way, this is not
		// an ordinary "may I run this tool" prompt.
		Tone: channelinteractions.ToneCritical,
		// The runner blocks the refused slot on this decision; omit Resume and the
		// applied envelope is dropped and the session waits out its full approval
		// timeout (category.go:119).
		Resume: channelinteractions.ResumeApproval,
		Park:   v1alpha1.AgentSessionPhaseAwaitingDecision,
		// Observable surface for the outstanding waiver. A channelsd-owned
		// approval condition (v1alpha1.IsApprovalCondition) so agentstatus.WriteOwned
		// persists it under the channelsd field manager.
		PendingCondition: v1alpha1.AgentSessionConditionPreconditionWaiverPending,
		// DecideResourceOwners is the LOAD-BEARING security row, exactly as
		// tool_approval and info_leakage: the bound handler writes the slot grant
		// on the refused RESOURCE UNCONDITIONALLY on approve, so the only thing
		// stopping an unauthorized clicker's grant is this policy re-checking
		// server-side that the clicker is an #owner of the resource named in the
		// request's Resources. A weaker decider would let a session owner
		// self-approve a waiver on someone else's resource.
		Deciders: channelinteractions.DecideResourceOwners,
		// The card content is fixed for the life of the request, so a
		// re-interacting owner gets the cached envelope republished verbatim.
		Resurface: channelinteractions.ResurfaceCached,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      CredentialLink,
		Tone:      channelinteractions.ToneRoutine,
		Park:      v1alpha1.AgentSessionPhaseAwaitingCredentials,
		Deciders:  channelinteractions.DecideRequester, // requester = the session starter; credential_link never receives an interaction_decision (out-of-band), so this is nominal
		Resurface: channelinteractions.ResurfaceRegenerate,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      WorkshopCredential,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideRequester, // requester = the builder-session participant the card is addressed to
		Resurface: channelinteractions.ResurfaceRegenerate,
		// Park intentionally omitted (non-parking, matches PortalAccess): the
		// credential belongs to the workshop bot's own identity, not the
		// builder session's, so there is nothing here for the builder to
		// halt for. No regenerator is bound for 5a (task 5 wires none), so a
		// re-interaction won't re-surface the card — the same accepted gap
		// CredentialUpdate has.
	})
	channelinteractions.Register(channelinteractions.Category{
		Name: CredentialUpdate,
		// ToneDegraded, where the sibling CredentialLink is ToneRoutine: this
		// card is raised because a credential the user ALREADY connected was
		// independently verified dead. Something failed, the session survives,
		// and the user can act — which is the tone's definition. A first-time
		// connection prompt has nothing broken behind it and stays routine.
		Tone:     channelinteractions.ToneDegraded,
		Park:     v1alpha1.AgentSessionPhaseAwaitingCredentials,
		Deciders: channelinteractions.DecideRequester, // requester = the credential's own user
		// ResurfaceRegenerate is the semantically-correct policy — the signed
		// link expires, so a re-surfaced card must be re-minted, not replayed
		// from cache — but NO regenerator is bound for it: internal/cmd/channelsd binds
		// one only for CredentialLink. A user re-interacting while parked here
		// therefore does not get the card back. Known gap, not an oversight.
		Resurface: channelinteractions.ResurfaceRegenerate,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      IdentityChoice,
		Tone:      channelinteractions.ToneRoutine,
		Park:      v1alpha1.AgentSessionPhaseAwaitingIdentityChoice,
		Resume:    channelinteractions.ResumeChoice,    // the gate needs WHICH identity was picked, not a yes/no
		Deciders:  channelinteractions.DecideRequester, // requester = the identity the prompt addressed (the session starter)
		Resurface: channelinteractions.ResurfaceCached, // the prompt's content never changes across a re-interaction
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      PortalAccess,
		Tone:      channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideRequester, // addressed to the recipient; no decision leg (static link)
		Resurface: channelinteractions.ResurfaceNone,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:      PermissionRequest,
		Tone:      channelinteractions.ToneWaiting,
		Deciders:  channelinteractions.DecideOwner, // the session owner (started_by) approves the join
		Resurface: channelinteractions.ResurfaceNone,
		Surface:   channelinteractions.SurfaceDMOnly, // the owner may not be in the requester's channel
	})
	channelinteractions.Register(channelinteractions.Category{
		Name: StartApproval,
		Tone: channelinteractions.ToneWaiting,
		// Platform admins only — the parked session has no owner or
		// started_by seat to derive an approver from, and a starter-granted
		// guest must not be able to admit other guests.
		Deciders:  channelinteractions.DecidePlatformAdmin,
		Resurface: channelinteractions.ResurfaceNone,
		Surface:   channelinteractions.SurfaceDMOnly, // an admin is rarely in the guest's channel
	})
	channelinteractions.Register(channelinteractions.Category{
		Name: ProviderErrorRetry,
		Tone: channelinteractions.ToneDegraded,
		// DecideParticipant: the Retry button is an addressee-less broadcast
		// posted to the session thread, so ANY active participant with interact
		// standing may click it (the pipe re-checks that standing server-side).
		// There is no single addressee to gate on.
		Deciders: channelinteractions.DecideParticipant,
		// One-shot in-thread post, never re-surfaced. Park stays "" because
		// AwaitingRetry is a runner/operator-owned phase, not an interaction
		// park state; Surface is unused for a participants broadcast.
		Resurface: channelinteractions.ResurfaceNone,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name: QueuedMessages,
		Tone: channelinteractions.ToneRoutine,
		// The ack is delivered to the requester (the mid-turn sender) as an
		// ephemeral-with-DM-fallback prompt.
		Surface: channelinteractions.SurfaceEphemeralDMFallback,
		// DecideParticipant: the ack is ADDRESSED to the requester, but the
		// "Interrupt & Send Now" button may be clicked by ANY active
		// participant with interact standing — the interrupt is a session-wide
		// "let this through now", not a private decision only the original
		// sender can make. The pipe re-checks CheckInteract server-side
		// regardless of who the prompt was delivered to.
		Deciders: channelinteractions.DecideParticipant,
		// Park stays "": this fires mid-turn while the session is genuinely
		// Running, so there is no phase to park in.
		Resurface: channelinteractions.ResurfaceNone,
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:   ContentInspection,
		Tone:   channelinteractions.ToneCritical,
		Resume: channelinteractions.ResumeApproval,
		Park:   v1alpha1.AgentSessionPhaseAwaitingDecision,
		// The generic park handler sets this condition True while parked, False
		// on resolve. Observable surface only; phase stays projected from the
		// runner's signed lifecycle log.
		PendingCondition: v1alpha1.AgentSessionConditionContentInspectionApprovalPending,
		// DecideApprovers with a nil resource set: content inspection is gated
		// on the session approve-set alone (the owner-derived approvers), NOT a
		// per-resource owner intersection — a suspected-injection flag has no
		// external resource counterparty to route consent to. The runner host
		// resolves the same approve-set into Audience.Approvers before it
		// prompts, and the decision pipe re-checks that standing server-side.
		Deciders: channelinteractions.DecideApprovers,
		// The prompt content (headline + inert excerpt) is fixed for the life
		// of the flag, so a re-interacting approver gets the cached envelope
		// republished verbatim — no publisher round-trip needed.
		Resurface: channelinteractions.ResurfaceCached,
		// Surface defaults to SurfaceEphemeralDMFallback. No PublicNote is set
		// on the payload: the flagged excerpt is untrusted suspected-injection
		// content and must never be posted where the whole conversation sees it.
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:   ToolApproval,
		Tone:   channelinteractions.ToneRoutine,
		Resume: channelinteractions.ResumeApproval,
		Park:   v1alpha1.AgentSessionPhaseAwaitingDecision,
		// The in-process e2e factory never reaches the operator's phase
		// projection, so this condition is its only observable.
		PendingCondition: v1alpha1.AgentSessionConditionToolApprovalPending,
		// DecideResourceOwners is the LOAD-BEARING security row: the bound
		// grant handler (BindToolApprovalHandler) writes the SpiceDB grant
		// tuple UNCONDITIONALLY on approve, so the ONLY thing stopping an
		// unauthorized clicker's grant from being written is this policy
		// re-checking server-side that the clicker is an #owner of the tool's
		// gated resource (request.Resources). A weaker decider
		// (DecideApprovers/DecideParticipant) would let a session owner
		// self-approve access to someone else's resource. When the tool_call
		// carries no resource, Resources is empty and the pipe folds to the
		// session approve-set — the same union CheckApproverAuthorized enforces.
		Deciders: channelinteractions.DecideResourceOwners,
		// The prompt content is fixed for the life of the request, so a
		// re-interacting owner gets the cached envelope republished verbatim.
		Resurface: channelinteractions.ResurfaceCached,
		// Surface = ephemeral-with-DM-fallback (default). The publisher sets
		// Audience.PublicNote — the only category that does — and the expiry
		// ticker keeps that public note's elapsed time current.
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:             InfoLeakage,
		Tone:             channelinteractions.TonePrivacy,
		Resume:           channelinteractions.ResumeApproval,
		Park:             v1alpha1.AgentSessionPhaseAwaitingDecision,
		PendingCondition: v1alpha1.AgentSessionConditionInfoLeakageApprovalPending,
		// DecideResourceOwners: only an #owner of the tainted data (Resources =
		// the taint data refs) may approve a share — data-owner-only standing,
		// NO session approve-set. Same load-bearing role as tool_approval's.
		Deciders:  channelinteractions.DecideResourceOwners,
		Resurface: channelinteractions.ResurfaceCached,
		// No-public-post invariant: the info_leakage publisher never sets
		// PublicNote. The proposed share and its recipient must never be posted
		// where the whole conversation can see them.
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:   DataSlotDisclosure,
		Tone:   channelinteractions.TonePrivacy,
		Resume: channelinteractions.ResumeApproval,
		// The PARENT parks: it is the session whose delegate call is blocked
		// waiting for this. The child does not exist yet — deliberately, since
		// starting it with the undecided slot omitted would hand it a partial
		// handoff it cannot distinguish from a smaller task.
		Park:             v1alpha1.AgentSessionPhaseAwaitingDecision,
		PendingCondition: v1alpha1.AgentSessionConditionInfoLeakageApprovalPending,
		// Only someone who can speak for the DATUM may clear it — the same
		// data-owner-only standing info_leakage uses, and for the same reason:
		// the session's own approve-set is the set this delegation would
		// disclose past.
		Deciders:  channelinteractions.DecideResourceOwners,
		Resurface: channelinteractions.ResurfaceCached,
		// No PublicNote, same invariant as info_leakage: the datum and the
		// agent about to receive it must not be posted where the whole
		// conversation can see them.
	})
	channelinteractions.Register(channelinteractions.Category{
		Name:             UserPreferenceConfirm,
		Tone:             channelinteractions.ToneRoutine,
		Resume:           channelinteractions.ResumeApproval,
		Park:             v1alpha1.AgentSessionPhaseAwaitingDecision,
		PendingCondition: v1alpha1.AgentSessionConditionPreferenceConfirmPending,
		// DecideRequester: answerable ONLY by the addressee — the turn author
		// whose preference is being saved. Deliberately not owner-derived: in
		// a shared thread the affected user, not the session owner, decides.
		// DecideRequester also refuses synthetic subjects, fail-closed.
		Deciders:  channelinteractions.DecideRequester,
		Resurface: channelinteractions.ResurfaceCached,
	})
}
