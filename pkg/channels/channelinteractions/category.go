// Package channelinteractions holds the semantic interaction-category
// registry: the single place a prompt type (tool approval, credential link,
// identity choice, …) is described. The relay, resurface machinery, pipeline
// decision pipe, and every channel kind dispatch generically over this
// registry — adding a category here requires ZERO per-channel-kind code.
//
// Named channelinteractions (not "interactions") to avoid colliding with
// pkg/channels/interact, the view-surface inbound-kind registry.
package channelinteractions

import (
	"fmt"
	"regexp"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// DeciderPolicy says who has standing to act on a category's interactions.
// Decision authz is enforced server-side in the pipeline decision pipe
// (fail-closed); this policy selects WHICH check runs.
type DeciderPolicy string

const (
	// DecideOwner: the session owner (agentsession#owner).
	DecideOwner DeciderPolicy = "owner"
	// DecideApprovers: the owner-derived approver set.
	DecideApprovers DeciderPolicy = "approvers"
	// DecideRequester: the identity the prompt was addressed to.
	DecideRequester DeciderPolicy = "requester"
	// DecideParticipant: any user with interact standing on the session
	// (CheckInteract) may decide. Used for participant-scoped prompts
	// (provider_error_retry, queued_messages) that anyone active can action.
	DecideParticipant DeciderPolicy = "participant"
	// DecideResourceOwners: any #owner of any resource named in the request's
	// Resources may decide (quorum = 1). Empty Resources ⇒ the session
	// approve-set gate; CheckApproverAuthorized folds both. Fail-closed when
	// the resource set is unrecoverable (no cached request AND no durable
	// record).
	DecideResourceOwners DeciderPolicy = "resource_owners"
	// DecidePlatformAdmin: only a holder of platform#start_session (the
	// platform-admin arm of the session-start area) may decide. Used for
	// start_approval, where the parked session has no owner, no started_by,
	// and no participants yet — deliberately NOT the class's own
	// agentclass#start_session, whose `starter` relation a previously-admitted
	// guest may hold: a guest allowed to start must not thereby admit other
	// guests.
	DecidePlatformAdmin DeciderPolicy = "platform_admin"
)

// ResumePolicy says how the runner's applied-envelope bridge wakes a gate that
// is blocked on this category's decision.
//
// The shapes differ in what the runner needs from the decision: an approval
// gate resumes on a yes/no, a choice gate resumes on WHICH action was picked.
type ResumePolicy string

const (
	// ResumeNone: the runner does not block on this category, so an applied
	// envelope for it is nothing to deliver. Declared, not inferred — see
	// Category.Resume.
	ResumeNone ResumePolicy = ""
	// ResumeApproval: a yes/no gate. Approved is true only for an approved
	// outcome; denied and expired both resume as false.
	ResumeApproval ResumePolicy = "approval"
	// ResumeChoice: an N-way pick. The chosen action, not a boolean, is what
	// the gate needs.
	ResumeChoice ResumePolicy = "choice"
)

// ResurfacePolicy says what happens when a user re-interacts with a session
// that has this category's interaction pending.
type ResurfacePolicy string

const (
	// ResurfaceCached: republish the cached request envelope verbatim.
	ResurfaceCached ResurfacePolicy = "cached"
	// ResurfaceRegenerate: ask the publisher to rebuild the request
	// (credential links re-mint their signed URL).
	ResurfaceRegenerate ResurfacePolicy = "regenerate"
	// ResurfaceNone: never resurfaced (fire-and-forget link cards).
	ResurfaceNone ResurfacePolicy = "none"
)

// nameRe enforces snake_case category names ("tool_approval").
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parkablePhases are the AgentSession phases an interaction may park a
// session in. A category with Park == "" does not park (link/notice cards).
var parkablePhases = map[string]bool{
	v1alpha1.AgentSessionPhaseAwaitingDecision:       true,
	v1alpha1.AgentSessionPhaseAwaitingCredentials:    true,
	v1alpha1.AgentSessionPhaseAwaitingIdentityChoice: true,
	v1alpha1.AgentSessionPhaseAwaitingRetry:          true,
}

// SurfaceType is a per-category delivery-surface hint. A fixed property of the
// category, so it lives on the registry row, not the per-request payload. Rich
// surfaces (Slack) read it to choose ephemeral-with-DM-fallback vs DM-only;
// text-floor surfaces ignore it.
type SurfaceType string

const (
	// SurfaceEphemeralDMFallback (the zero value): post ephemeral in the
	// bound channel/thread; on user-not-in-channel, fall back to a DM.
	SurfaceEphemeralDMFallback SurfaceType = ""
	// SurfaceDMOnly: always open a DM and post there (a single named
	// recipient who may not be in the channel — e.g. a session owner).
	SurfaceDMOnly SurfaceType = "dm_only"
)

// Category describes one interaction type. Rows are PURELY declarative:
// decision plumbing does NOT live on this struct — handlers close over their
// dependencies and attach at process start via Bind/HandlerFor in decision.go,
// the registry-vs-DI split used across the repo.
type Category struct {
	// PlanConsent permits this category to be included in a plan approval.
	PlanConsent bool
	// IncludesConsents permits a plan card to carry exact child requests.
	IncludesConsents bool
	// Name is the registry key, snake_case ("tool_approval").
	Name string
	// Park is the AgentSession phase this category parks the session in
	// while awaiting the user, or "" for non-parking categories.
	Park string
	// Deciders selects the server-side standing check for decisions.
	Deciders DeciderPolicy
	// Resurface says how a pending interaction re-reaches a returning user.
	Resurface ResurfacePolicy
	// Surface hints how rich surfaces deliver this category's prompt.
	Surface SurfaceType
	// Resume says how the RUNNER wakes when this category's decision is applied.
	//
	// It lives here because the runner's applied-envelope bridge used to branch
	// on a hardcoded list of category names, with an unlisted category falling
	// through to a silent return. Registering plan_phase and plan_amendment
	// therefore produced a session that published its prompt, accepted the
	// click, and then waited out its full approval timeout — a hang whose cause
	// was nowhere near the code that caused it, and which read as a harness
	// defect for long enough to park two test bundles.
	//
	// Empty means ResumeNone: the category is declared as one the runner does
	// not wait on. That is a claim the registration makes, which is what lets
	// the bridge tell "deliberately not resuming" apart from "nobody wired it".
	Resume ResumePolicy
	// PendingCondition is the AgentSession status condition type the generic
	// park handler sets True while this category's interaction is parked, and
	// False once the category's last entry resolves. It is an OBSERVABLE
	// surface only, NOT a phase authority: phase=AwaitingDecision stays derived
	// from the runner's signed lifecycle projection. Empty for non-parking /
	// non-approval categories. The value must be a channelsd-owned approval
	// condition (v1alpha1.IsApprovalCondition) so agentstatus.WriteOwned
	// persists it under the channelsd field manager.
	PendingCondition string

	// Notice marks this row as a ZERO-ACTION notice rather than a prompt: a
	// one-way message (an error, a denial, a lifecycle event) the user reads
	// and cannot act on. Validate then enforces the notice contract instead of
	// the prompt one — no Deciders, no Park, no PendingCondition,
	// ResurfaceNone — and requires a Tone.
	//
	// Saying "this has no decision leg" explicitly is what lets Validate REJECT
	// a decider policy on such a row. Without it a notice would have to name
	// some nominal policy it never invokes, indistinguishable from a real one.
	Notice bool
	// Tone is what kind of concern this category carries. REQUIRED on every
	// row, prompt and notice alike, so the choice is reviewable in one file and
	// the vocabulary stays closed. See tone.go.
	Tone Tone
	// Terminal marks a message after which nothing further will happen: the
	// session is dead, or the request is refused permanently. Surfaces draw it
	// distinctly (Slack: a square chip rather than a circle) so "stop waiting"
	// is legible without reading the copy.
	//
	// Notices only — Validate rejects it on a prompt, which by definition is
	// still awaiting a decision. Most notices are NOT terminal.
	Terminal bool
	// Glyph is an optional SEMANTIC override for the mark drawn beside the
	// lead. It replaces the tone chip entirely, so it costs both the colour and
	// the terminal/non-terminal shape — see tone.go for when that trade is
	// acceptable.
	Glyph Glyph
}

// Validate reports whether the category is well-formed. Registration panics
// on invalid categories: a malformed category is a programmer error that
// must surface at process start, never at dispatch time.
func (c Category) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("interaction category: name must not be empty")
	}
	if !nameRe.MatchString(c.Name) {
		return fmt.Errorf("interaction category %q: name must be snake_case", c.Name)
	}
	switch c.Resume {
	case ResumeNone, ResumeApproval, ResumeChoice:
	default:
		return fmt.Errorf("interaction category %q: unknown resume policy %q", c.Name, c.Resume)
	}
	if c.Park != "" && !parkablePhases[c.Park] {
		return fmt.Errorf("interaction category %q: unknown park phase %q", c.Name, c.Park)
	}
	// Tone and Glyph apply to both families: a row that declines to declare a
	// tone falls back to whatever the renderer defaults to, which is how a
	// vocabulary drifts.
	if !c.Tone.valid() {
		return fmt.Errorf("interaction category %q: unknown tone %q", c.Name, c.Tone)
	}
	if !c.Glyph.valid() {
		return fmt.Errorf("interaction category %q: unknown glyph %q", c.Name, c.Glyph)
	}
	// A notice row and a prompt row are two different contracts. Branch once
	// here rather than threading `if c.Notice` through every check.
	if c.PlanConsent && (c.Notice || c.Deciders != DecideRequester || c.Resume != ResumeNone || c.Resurface != ResurfaceCached) {
		return fmt.Errorf("interaction category %q: plan consent requires a cached requester decision without runner resume", c.Name)
	}
	if c.IncludesConsents && (c.Notice || c.Resume != ResumeApproval || c.Deciders != DecideApprovers) {
		return fmt.Errorf("interaction category %q: composed consent requires an approver plan gate", c.Name)
	}
	if c.Notice {
		return c.validateNotice()
	}
	if err := c.validatePrompt(); err != nil {
		return err
	}
	switch c.Resurface {
	case ResurfaceCached, ResurfaceRegenerate, ResurfaceNone:
	default:
		return fmt.Errorf("interaction category %q: unknown resurface policy %q", c.Name, c.Resurface)
	}
	switch c.Surface {
	case SurfaceEphemeralDMFallback, SurfaceDMOnly:
	default:
		return fmt.Errorf("interaction category %q: unknown surface %q", c.Name, c.Surface)
	}
	return nil
}
