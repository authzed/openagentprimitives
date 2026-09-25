// Package sessionrelease is the session_release interaction category: the
// owner-gated approval card that clears a SessionHold (see
// pkg/apis/v1alpha1/sessionhold_types.go) and hands a forensically-held
// session back to its owner.
//
// Registered the same way every one of the 33 production categories under
// pkg/channels/channelinteractions/categories/ is: a plain
// channelinteractions.Category row (see categories.go's registerPrompts for
// the sibling shape this mirrors), Registered from init() so importing this
// package is what makes the category visible. It lives in its own package
// rather than being folded into categories.go because its card-building logic
// (BuildCard/CardInput/Card below) needs a home, and every other category
// that has one — plangate's plan_phase/plan_amendment — keeps that logic in
// ITS OWN package (pkg/authz/plangate), not in categories.go either.
//
// The closest sibling for the REGISTRY ROW is PermissionRequest
// (categories.PermissionRequest, the session-join grant): both are
// DecideOwner, both park no AgentSession phase, both resume no runner. A held
// session has already been parked by the AgentSession reconciler's own
// lifecycle.Held transition and has no live runner (it was reaped) — there is
// nothing left for THIS category to park or resume.
//
// The card carries NO agent-authored field — a deliberate divergence from
// CredentialUpdateRequest, whose spec.why is the agent's own ask. Here the
// agent is the SUBJECT of a human's decision, not the requester, so it gets no
// voice at all: every BuildCard input is platform-authored (trip reason,
// source, evidence, snapshot handle).
package sessionrelease

import (
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// CategoryName is the channelinteractions registry key.
const CategoryName = "session_release"

func init() {
	channelinteractions.Register(channelinteractions.Category{
		Name: CategoryName,
		// ToneCritical: matches content_inspection, the other category that
		// stops a session over a suspected problem rather than an ordinary ask
		// — a categorically different decision must also LOOK different.
		Tone: channelinteractions.ToneCritical,
		// DecideOwner: releasing a forensic hold is the session owner's call,
		// the same standing permission_request's join grant uses.
		Deciders: channelinteractions.DecideOwner,
		// Park stays "": the AgentSession is already parked at PhaseHeld by the
		// lifecycle machine (Task 1/4), and Held is not one of the interaction
		// park phases — there is no AwaitingXYZ phase for this category to
		// additionally occupy.
		Park: "",
		// ResumeNone: nothing in a runner is blocked on this decision — a held
		// session's runner has been reaped, so there is no runner to resume.
		Resume: channelinteractions.ResumeNone,
		// ResurfaceNone, matching permission_request: the card is short,
		// platform-authored text with nothing that needs regenerating (unlike
		// a signed credential link) and nothing expensive to recompute if a
		// resurface is ever added.
		Resurface: channelinteractions.ResurfaceNone,
		// DMOnly: the owner may not be present in whatever channel/thread the
		// held session was attached to — same reasoning as permission_request.
		Surface: channelinteractions.SurfaceDMOnly,
	})
}

// FailsClosedOnTimeout reports that an unanswered release request is NOT a
// release.
//
// channelinteractions.Category has no timeout field of its own — nothing
// registered there auto-resolves on a timer; every existing timeout (e.g.
// permission_request's TimeoutPermissionRequest,
// pkg/channels/channelsd/pipeline/decision.go) is a category-specific
// mechanism, not a registry property. This function documents session_release's
// side of that mechanism: the SessionHold reconciler
// (pkg/controllers/sessionhold) has no code path that transitions
// status.phase to Released except an explicit approved Decide call, so a card
// nobody acts on simply leaves the hold Active, forever, by construction —
// nothing needs to actively expire it.
func FailsClosedOnTimeout() bool { return true }

// CardInput is everything BuildCard needs. Every field is platform-authored —
// see the package doc for why the agent gets none.
type CardInput struct {
	// SessionName names the held AgentSession.
	SessionName string
	// Reason is the platform-authored trip text (SessionHold.spec.reason).
	Reason string
	// Source is "manual" or "tripper/<name>" (SessionHold.spec.source).
	Source string
	// Evidence, when set, names what was observed (e.g. which handles were
	// refused, in which phase). Optional — a manual trip may carry none.
	Evidence string
	// SnapshotHandle names the workspace snapshot taken at trip time, empty
	// until the snapshot Job completes.
	SnapshotHandle string
}

// Card is a rendered session_release approval card.
type Card struct {
	// Lead is the headline, carrying the severity marker so a channel with no
	// styling affordance still shows the signal as text.
	Lead string
	// Body is the platform-authored supporting text.
	Body string
	// Fields are the platform-authored facts, keyed by name ("session",
	// "reason", "source", "evidence", "snapshot") — never "why": on this card
	// the agent is the SUBJECT of the decision, not the requester, so it gets
	// no voice.
	Fields map[string]string
	// Severity is always "severe" — plangate's existing routine/elevated/severe
	// scale, reused rather than inventing a parallel one.
	Severity string
}

// BuildCard renders a session_release approval card. Every input is
// platform-authored; there is no agent-authored field (see the package doc).
func BuildCard(in CardInput) Card {
	fields := map[string]string{
		"session": in.SessionName,
		"reason":  in.Reason,
		"source":  in.Source,
	}
	body := fmt.Sprintf("Session %s was frozen for human review.\n\nReason: %s\nSource: %s",
		in.SessionName, in.Reason, in.Source)
	if in.Evidence != "" {
		fields["evidence"] = in.Evidence
		body += "\nEvidence: " + in.Evidence
	}
	if in.SnapshotHandle != "" {
		fields["snapshot"] = in.SnapshotHandle
		body += "\nWorkspace snapshot: " + in.SnapshotHandle
	}
	lead := "Session held for review"
	if m := plangate.Severe.Marker(); m != "" {
		lead = m + " " + lead
	}
	return Card{
		Lead:     lead,
		Body:     body,
		Fields:   fields,
		Severity: string(plangate.Severe),
	}
}
