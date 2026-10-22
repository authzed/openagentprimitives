package runner

// plangate_notice.go tells a human when their plan approval did not take
// effect.
//
// The gate fails closed by design: if it cannot establish which resources the
// approver is entitled to share, it records nothing rather than granting
// something nobody authorized. That half is right. What was missing is the
// other half — saying so. An approval that vanishes silently leaves the person
// who clicked Approve looking at the identical card again, with no signal that
// anything went wrong, so they approve again, and again. The gate looks like it
// is working; the human is the one being made to repeat themselves.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// approverIDOf renders a principal for a LOG line — never for a chat surface,
// and never for an authorization call. Best effort: a principal that cannot
// canonicalize still needs to appear in the log that says so.
func approverIDOf(p identity.Principal) string {
	if e := p.Email(); e != "" {
		return string(e)
	}
	if id := p.ExternalID(); id != "" {
		return string(id)
	}
	if c, err := p.Canonical(); err == nil {
		return c.String()
	}
	return "(unidentified)"
}

// approvalNotice is the copy one failed approval publishes to its approvers.
type approvalNotice struct {
	category string
	lead     string
	nextStep string
}

// discardedApprovalNotice is the copy for an approval the gate could not RECORD
// at all — the standing lookup that bounds what the approver may delegate
// failed, so nothing was written and the card will be asked again. "Nothing was
// granted, try once more" is true here and only here: once a decision is
// recorded, the bind-time outcomes below apply instead.
func discardedApprovalNotice() approvalNotice {
	return approvalNotice{
		// InternalError is the "we broke, retrying is a remedy" category.
		category: categories.InternalError,
		lead: "Your approval could not be applied, so nothing was granted and the plan " +
			"has not started. This is a fault on our side, not a refusal.",
		// Bounded rather than "retry"/"don't retry" — one more attempt
		// distinguishes a transient blip from a persistent one; a third never does.
		nextStep: "Try approving once more. If it fails again, report it — nothing " +
			"has been granted either way.",
	}
}

// publishPlanGateApprovalFailed tells the session that an approval was received
// and did not take effect as shown.
//
// Best-effort by the same rule as every other gate write — a notice that fails
// to publish must not take down the turn — but it must never be silent, so a
// failure to deliver the failure is itself logged.
//
// The text names no internal machinery. "The permission service rejected the
// lookup" is not something the person clicking Approve can act on, and this
// surface is a chat window, not an operator console; the operator detail is in
// the log line beside this call.
func (h *runnerHost) publishPlanGateApprovalFailed(ctx context.Context, cause error, n approvalNotice) {
	if h.l == nil || h.l.InteractionRequestPublish == nil {
		return
	}
	sessNS, sessName := h.l.SessionKey.Namespace, h.l.SessionKey.Name
	if sessNS == "" && sessName == "" {
		sessNS, sessName = h.sess.Namespace, h.sess.Name
	}
	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: sessNS, Name: sessName},
		Category:        n.category,
		RequestRef:      newRequestID(),
		Lead:            n.lead,
		// Required of every degraded-tone notice, and the reason the rule exists:
		// without it this says something went wrong and leaves the reader holding
		// an unanswered card.
		NextStep: n.nextStep,
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers},
	}
	env, err := channelevents.BuildEnvelope(sessNS, sessName, channelevents.KindInteractionRequest, pl)
	if err != nil {
		h.logApprovalNoticeFailure(sessNS, sessName, fmt.Errorf("build envelope: %w", err), cause)
		return
	}
	if err := h.l.InteractionRequestPublish(ctx, sessNS, sessName, env); err != nil {
		h.logApprovalNoticeFailure(sessNS, sessName, err, cause)
	}
}

func (h *runnerHost) logApprovalNoticeFailure(ns, name string, err, cause error) {
	slog.Default().Info("plan_gate: could not tell the approver that their approval was discarded; "+
		"they will see the same card again with no explanation",
		"session", ns+"/"+name, "publishErr", err.Error(), "cause", cause.Error())
}

// tierTone maps a CardLine's Impact — "readonly", "readwrite" or "external",
// the plan-gate's own vocabulary (permsurface.Descriptor.StateImpact) — onto
// the wire Tone that carries the same three tiers. An empty or unrecognized
// Impact (the resource lines under a phase never set it) yields the zero
// Tone rather than guessing: no Impact means the translation has nothing to
// carry, not that the line is readonly by default.
func tierTone(impact string) channelevents.Tone {
	switch impact {
	case "readonly":
		return channelevents.ToneReadonly
	case "readwrite":
		return channelevents.ToneReadwrite
	case "external":
		return channelevents.ToneExternal
	default:
		return ""
	}
}

// planGateItems projects the card's phases onto the transport's generic
// structured-field shape.
//
// A translation and nothing more: no fact appears here that the card did not
// compute, and none is dropped. The card owns what an approval says; this owns
// only how it crosses the wire.
func planGateItems(c plangate.Card) []channelevents.InteractionItem {
	if len(c.Phases) == 0 {
		return nil
	}
	out := make([]channelevents.InteractionItem, 0, len(c.Phases)+1)
	for _, ph := range c.Phases {
		item := channelevents.InteractionItem{Text: ph.Title}
		for _, p := range ph.Permissions {
			tone := tierTone(p.Impact)
			if tone == "" && p.External {
				// Impact is the authoritative source (card.go derives both from
				// the same StateImpact lookup, so in practice they never
				// disagree), but a caller that only set External still gets the
				// tier its own flag names rather than no tone at all.
				tone = channelevents.ToneExternal
			}
			line := channelevents.InteractionItem{
				Text: p.Text,
				Tone: tone,
				// The raw handle rides as a hint a surface may reveal on demand
				// (a hover tooltip) — never as part of Text or Detail, which are
				// what a reader is shown by default.
				Hint: p.Handle,
			}
			if p.External {
				// The reason the card is worth reading. Carried as tone rather
				// than as a suffix so a surface can make it unskimmable, and
				// restated here as detail so the meaning survives wherever tone
				// alone can't (a copy-paste, a plain-text log line).
				line.Detail = "leaves this session"
			}
			item.Items = append(item.Items, line)
		}
		for _, r := range ph.Resources {
			// Icon and Href ride straight over the wire: the card already
			// decided them (resolveResourceLine, gated on eligibleHref), and
			// this translation adds nothing — in particular it never
			// recomputes Href from Detail, which is what keeps text == href a
			// property of construction all the way to the wire rather than
			// something this step could accidentally break.
			item.Items = append(item.Items, channelevents.InteractionItem{
				Text: "reaches " + r.Text, Detail: r.Detail, Icon: r.Icon, Href: r.Href,
			})
			if r.MovedFrom != "" {
				// The cost half of a slot move, as its own line: approving
				// repoints the pin and revokes the displaced instance. External
				// tone because the approver loses something, and a surface must
				// not render that at the weight of a read.
				item.Items = append(item.Items, channelevents.InteractionItem{
					Text: "moves the pin and revokes this session's access to " + r.MovedFrom,
					Tone: channelevents.ToneExternal,
				})
			}
			if r.PinnedImmovable != "" {
				// A `rebind: never` slot pinned elsewhere: the named instance
				// cannot be granted here. Carried as its own line so a surface
				// cannot render the resource above as grantable — external tone
				// because it is a hard limit the approver must not skim past.
				item.Items = append(item.Items, channelevents.InteractionItem{
					Text: "cannot be granted — this session is pinned to " + r.PinnedImmovable +
						" for its life; start a new session to reach " + r.Detail,
					Tone: channelevents.ToneExternal,
				})
			}
		}
		out = append(out, item)
	}
	if c.Coverage != "" {
		// What one click buys, kept as its own line rather than folded into the
		// last phase — it is a statement about the whole card.
		out = append(out, channelevents.InteractionItem{
			Text: c.Coverage, Tone: channelevents.ToneMuted,
		})
	}
	return out
}

// slotOutcome is how one resource type fared across an approval's binds.
type slotOutcome int

const (
	slotBound slotOutcome = iota
	// slotFault: a store fault, or an approved move whose pin drifted since the
	// card. Nothing changed for the type; the agent asks again when it needs it.
	slotFault
	// slotMoveCommitted: the approved move landed — the displaced instance's
	// access is revoked — but the grant on the new instance did not.
	slotMoveCommitted
	// slotRefused: the type is committed to a different instance; an answer,
	// not a fault, and re-approving will not change it.
	slotRefused
)

// approvalOutcome aggregates, PER RESOURCE TYPE, how one approval's binds
// landed across every phase it covers, so the approver gets ONE notice that is
// true for the approval as a whole.
//
// Per type because GrantSlots is per-type partitioned: one type can be refused
// while another binds and a third blips, and a single label for the whole call
// lies about at least two of them. Across phases because a plan-scoped approval
// binds once per covered phase: publishing per phase repeated the notice for
// each, and could contradict itself — a "move committed but not granted" from
// phase 1 beside a later phase whose same-instance bind then wrote that grant.
//
// Merge rule, in phase order: a refusal is sticky (the instance it named was
// not granted, whatever a later phase did); otherwise the LATEST outcome for
// the type wins, so a later successful bind supersedes an earlier fault or
// committed-move.
//
// A type held out by a precondition (BindApproved drops it without error) reads
// as bound here; it escalates to the waiver card on its own, which is the
// notice it needs.
type approvalOutcome struct {
	order  []string
	byType map[string]slotOutcome
	causes []error
}

// record folds one phase's BindApproved result in. It reports false when err
// carries no per-type slot outcome at all (a session_scope write failure before
// GrantSlots ran); the caller keeps that log-only, as before.
func (o *approvalOutcome) record(bindings []authz.SlotBinding, err error) bool {
	if o.byType == nil {
		o.byType = map[string]slotOutcome{}
	}
	failures := authz.SlotBindFailures(err)
	if err != nil && len(failures) == 0 {
		return false
	}
	if err != nil {
		o.causes = append(o.causes, err)
	}
	this := map[string]slotOutcome{}
	for _, b := range bindings {
		this[b.ResourceType] = slotBound
	}
	for _, f := range failures {
		k := slotFault
		switch f.Kind {
		case authz.SlotFailureRefused:
			k = slotRefused
		case authz.SlotFailureMoveCommitted:
			k = slotMoveCommitted
		case authz.SlotFailureFault, authz.SlotFailureDrifted:
		}
		for _, rt := range f.ResourceTypes {
			this[rt] = k
		}
	}
	for _, b := range bindings {
		o.merge(b.ResourceType, this[b.ResourceType])
		delete(this, b.ResourceType)
	}
	for rt, k := range this { // a failure naming a type no binding did (defensive)
		o.merge(rt, k)
	}
	return true
}

func (o *approvalOutcome) merge(rt string, k slotOutcome) {
	prev, seen := o.byType[rt]
	if !seen {
		o.order = append(o.order, rt)
	}
	if seen && prev == slotRefused {
		return
	}
	o.byType[rt] = k
}

func (o *approvalOutcome) typesWith(k slotOutcome) []string {
	var out []string
	for _, rt := range o.order {
		if o.byType[rt] == k {
			out = append(out, rt)
		}
	}
	return out
}

// cause joins every recorded error, for the log line beside the notice.
func (o *approvalOutcome) cause() error { return errors.Join(o.causes...) }

// notice builds the one approver notice the outcome warrants; ok=false when
// every type bound.
//
// The primary copy follows the most consequential fact — a committed move (an
// instance's access was REMOVED), then a refusal (an answer), then a fault —
// and any other fact present is appended to the next step, so nothing the
// approver needs is dropped and nothing claims more than happened. In
// particular no bind-time copy says "nothing was granted": the decision is
// recorded and the other types may well have bound.
//
// The refusal route follows the REFUSED types' policy only (authz.AllRebindNever
// over the refusals, never over every binding): when every refused slot is
// `rebind: never`, no amendment can move it and the only route is a new
// session.
func (o *approvalOutcome) notice() (approvalNotice, bool) {
	committed, refused, faulted := o.typesWith(slotMoveCommitted), o.typesWith(slotRefused), o.typesWith(slotFault)
	var n approvalNotice
	switch {
	case len(committed) > 0:
		n = approvalNotice{
			category: categories.InternalError,
			lead: "Your approval moved this session to the new target for " + typeList(committed) +
				" and removed its access to the previous one, but access to the new target could not be granted.",
			nextStep: "The agent will ask again when it reaches the new target. If it fails again, report it.",
		}
	case len(refused) > 0:
		// An answer, not a fault: SlotBindRefused, not InternalError — the
		// category is what a surface reads to tell "we erred, try again" from
		// "this is the answer".
		n = approvalNotice{
			category: categories.SlotBindRefused,
			lead: "This session is already committed to a different target for " + typeList(refused) +
				", so the instance your approval named there was not granted. Other instances the plan " +
				"named may have been granted; that one was not.",
		}
		if authz.AllRebindNever(authz.SlotPinRefusals(o.cause())) {
			n.nextStep = "This commitment is fixed for the session's life and cannot be moved. " +
				"Start a new session to work on the instance you wanted. Re-approving this card will not change it."
		} else {
			n.nextStep = "To reach the instance you wanted, the plan must name it so an approved amendment " +
				"moves the commitment to it; otherwise start a new session for it. Re-approving this card will not move it."
		}
	case len(faulted) > 0:
		n = approvalNotice{
			category: categories.InternalError,
			lead: "Your approval was recorded but could not be fully applied, so some or all of the access " +
				"it covers (" + typeList(faulted) + ") was not granted. This is a fault on our side, not a refusal.",
			nextStep: "The agent will ask again when it reaches anything that was not granted. If that fails too, report it.",
		}
	default:
		return approvalNotice{}, false
	}
	if len(committed) > 0 && len(refused) > 0 {
		n.nextStep += " Separately, this session is committed to a different target for " + typeList(refused) +
			", so the instance named there was not granted."
	}
	if (len(committed) > 0 || len(refused) > 0) && len(faulted) > 0 {
		n.nextStep += " Separately, access to " + typeList(faulted) +
			" could not be granted because of a fault on our side; the agent will ask again when it reaches it."
	}
	return n, true
}

func typeList(types []string) string { return strings.Join(types, ", ") }

// conflictingMoveTargets returns, per single-occupancy resource type, the
// distinct instances an approval would MOVE the pin to when there is more than
// one. A plan whose phases move one pin to two different instances cannot be
// honored — a session holds one — and executing it phase by phase commits the
// first move (revoking the prior instance) and then fails the second, so the
// approval must be refused whole, before anything is written.
func conflictingMoveTargets(phases [][]authz.SlotBinding) map[string][]string {
	targets := map[string][]string{}
	for _, bindings := range phases {
		for _, b := range bindings {
			if b.PriorID.IsZero() || b.Occupancy == authz.SlotOccupancyMulti {
				continue
			}
			id := b.ResourceID.String()
			if !slices.Contains(targets[b.ResourceType], id) {
				targets[b.ResourceType] = append(targets[b.ResourceType], id)
			}
		}
	}
	for rt, ids := range targets {
		if len(ids) < 2 {
			delete(targets, rt)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return targets
}

// moveConflictNotice is the copy for an approval refused whole by
// conflictingMoveTargets. Nothing was recorded or written, so the card will be
// asked again — and will conflict again until the plan changes, which is the
// route it names.
func moveConflictNotice(conflict map[string][]string) approvalNotice {
	types := make([]string, 0, len(conflict))
	for rt := range conflict {
		types = append(types, rt)
	}
	slices.Sort(types)
	return approvalNotice{
		category: categories.SlotBindRefused,
		lead: "This plan moves " + typeList(types) + " to more than one target, but a session can be " +
			"committed to only one, so your approval was not applied and nothing was granted.",
		nextStep: "Ask the agent to revise the plan so it works on one target per session, or split the " +
			"work across separate sessions. Re-approving this card will not change it.",
	}
}
