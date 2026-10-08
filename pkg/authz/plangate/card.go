package plangate

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

const (
	// maxWhyLen caps the agent's justification. Generous — the point is to stop
	// a runaway or hostile agent from producing an unrenderable card, not to
	// shape normal writing.
	maxWhyLen = 2000

	truncationMarker = " […truncated]"
	foldMarker       = "…and more"

	missingWhy = "(the agent gave no justification)"
)

// Card is a rendered plan-gate approval, in three fields with three different
// trust levels:
//
//	What  computed from the frozen plan and the surface        TRUSTED
//	Why   the agent's own words                                UNTRUSTED
//	When  computed from phase indices                          TRUSTED
//
// Keeping them visibly separate is the point. What is generated independently
// of anything the agent wrote, so a prompt-injected agent cannot author the
// sentence describing what it is asking for — it can only author Why, which is
// labelled as its claim.
type Card struct {
	// Lead is the headline, carrying the severity marker so a channel with no
	// styling affordance still shows the signal as text.
	Lead string `json:"lead"`

	What      string `json:"what"`
	Why       string `json:"why"`
	When      string `json:"when"`
	Approvers string `json:"approvers,omitempty"`
	Severity  string `json:"severity"`

	// Phases is What as STRUCTURE — the same facts the prose states, in a shape
	// a surface can lay out instead of print.
	//
	// Both, and they must agree. What is the fallback every surface can render;
	// Phases is what a surface with a design system should render instead. They
	// are built from one walk of the frozen plan for exactly that reason: two
	// renderers reading the same plan separately is how a card ends up saying
	// different things in Slack and in a browser.
	//
	// Same trust level as What: computed from the frozen plan and the
	// permission surface, never from anything the agent wrote.
	Phases []CardPhase `json:"phases,omitempty"`

	// Coverage is the sentence stating what one click actually buys — whether
	// this is the last prompt or merely the first. Carried separately because it
	// is a conclusion about the whole card, not a line inside a phase.
	Coverage string `json:"coverage,omitempty"`
}

// CardPhase is one phase as the approver sees it: what it may do, and to what.
//
// Titled by INDEX, never by the agent's label — the label is agent-authored and
// belongs in Why, attributed. See buildWholePlanWhat.
type CardPhase struct {
	Title       string     `json:"title"`
	Permissions []CardLine `json:"permissions,omitempty"`
	Resources   []CardLine `json:"resources,omitempty"`
}

// CardLine is one permission or one resource within a phase.
type CardLine struct {
	// Text is the human-readable description — a permission's describe form, or
	// a resource's type.
	Text string `json:"text"`

	// Detail is the instance a resource line names, empty when the phase
	// deferred it.
	Detail string `json:"detail,omitempty"`

	// External marks reach whose effects leave the session and cannot be
	// undone. Structural rather than a suffix on Text, so a surface can make it
	// unskimmable rather than hoping the reader parses a parenthetical.
	External bool `json:"external,omitempty"`

	// Impact is the state-impact tier this line carries — "readonly",
	// "readwrite" or "external" — so a surface can encode blast radius rather
	// than rendering a read and an irreversible push at the same weight.
	// DERIVED from permsurface.Descriptor.StateImpact, never declared: that is
	// the single source of truth and a second one would drift.
	Impact string `json:"impact,omitempty"`

	// Handle is the raw wire-format handle, carried for a surface that can show
	// it on demand (a tooltip). It is NEVER part of the visible line: Text is
	// what a human decides on.
	Handle string `json:"handle,omitempty"`

	// Icon names a glyph from the closed registry (knownIcon) for a RESOURCE
	// line, declared on the type's Display. Empty when undeclared or
	// unrecognized — resolveResourceLine drops an unknown name before it ever
	// reaches this field, so a surface never has to re-validate it.
	Icon string `json:"icon,omitempty"`

	// Href, when non-empty, is a SAFE link target for a RESOURCE line: the
	// SAME string as Detail, byte-identical, because resourcedisplay.EligibleHref
	// only ever returns its input unchanged or nothing at all. A surface renders
	// it as a link whose visible text is the href itself — never Text, never a
	// separately-held copy — so text == href holds by construction rather
	// than by convention. https only; a non-URL, non-https, or ineligible
	// Detail leaves this empty and Detail renders as plain text.
	Href string `json:"href,omitempty"`

	// MovedFrom, on a RESOURCE line, is the display label of the instance this
	// approval displaces: the session is pinned to it on a single-occupancy
	// slot, and approving moves the pin to Detail and revokes this session's
	// access to MovedFrom. Structural rather than folded into Detail so Detail
	// stays the instance itself (and Href stays byte-identical to it), and so a
	// surface renders the revocation as its own line instead of losing it.
	MovedFrom string `json:"movedFrom,omitempty"`
}

func (c Card) JSON() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("plangate: marshal card: %w", err)
	}
	return string(b), nil
}

// CardInput is everything BuildCard needs. Every field except the plan's own
// Why strings is computed by the runtime.
type CardInput struct {
	Plan       Plan
	PhaseIndex int
	Severity   Severity

	// Surface resolves handles to their state impact, so external reach can be
	// called out. Optional; without it handles render without that annotation.
	Surface []permsurface.Descriptor

	// PermissionTitles maps each declared permission's "<resourceType>/<permission>"
	// key to the human phrase the AgentClass controller published on
	// status.resolvedPermissionTitles (read via runner.PermissionTitlesOf). A
	// pair missing from the map has no declared title, and the line falls back
	// to a DETOKENIZED handle (Descriptor.DescribeWithTitle) rather than to
	// Describe()'s tool-anchored form. Optional; nil behaves exactly like every
	// pair being undeclared.
	PermissionTitles map[string]string

	// ResourceDisplays maps each resource type to the presentation declared on
	// its SpiceDBResource.Display, published on
	// status.resolvedResourceDisplays (read via runner.ResourceDisplaysOf). A
	// type missing from this map has no declared display, and its resource
	// lines fall back to the wire type name with no icon and no link —
	// exactly the pre-existing behavior. Optional; nil behaves exactly like
	// every type being undeclared.
	ResourceDisplays map[string]ResourceDisplay

	// WholePlan renders EVERY phase in one card instead of just PhaseIndex, and
	// prices the card at the worst phase in it.
	//
	// This is the plan-scoped decision. Per-phase cards asked serially and, at
	// the moment of the first one, said nothing about the push two phases later
	// — so a human cleared "read-only recon" without being shown what they were
	// starting. Severity follows: approving this authorizes the push, so it must
	// be priced as a push rather than as the read that happens to come first.
	WholePlan bool

	// Approvers is the pre-rendered approver population.
	Approvers string

	// MaxSingleCardHandles folds a long ceiling on a ROUTINE card. Ignored for
	// Elevated and Severe, which never fold. Zero ⇒ no folding.
	MaxSingleCardHandles int

	// AddedHandles, when non-empty, makes this an AMENDMENT card: the What
	// states what is being ADDED rather than restating the whole ceiling. An
	// approver deciding a widening needs to see the delta, not hunt for it in a
	// list they already approved.
	//
	// A SET, not one handle, because the two ways a widening reaches a human
	// differ only in how many additions they carry: a call denied at dispatch
	// contributes exactly one, and a re-plan contributes the whole delta at
	// once. Batching is the dominant fatigue fix — N additions must cost one
	// decision, not N — and it is only expressible if the card can hold N.
	AddedHandles []string

	// AddedSlots are the resources this widening asks to touch beyond what was
	// already granted — WITH their instances.
	//
	// Separate from AddedHandles because they are separate axes and a widening
	// can be either, both, or neither: a re-plan that adds only a slot changes
	// no handle at all, yet Widens() is true and a human is asked. Without this
	// the card that reaches them would describe nothing that changed.
	//
	// Slots rather than bare type names, and that is load-bearing now that a
	// re-POINT is a widening: an approval cleared for one company, re-aimed at
	// another, adds no type at all. Carrying types alone rendered the card as
	// "crm_company — (no target named yet)", asking a human to clear a resource
	// it declined to name — while the plan named it perfectly well. A card that
	// misdescribes what it grants is worse than one that never asks.
	AddedSlots []Slot

	// AgentJustification is the agent's stated reason for an amendment. Carried
	// separately from the plan's own Why fields because it arrives per-call via
	// `_reason`, and it is escaped on the same path — untrusted either way.
	AgentJustification string

	// SlotStanding maps each slot's resource type to how an approval on it gets
	// its authority: spiceboxv1alpha1.StandingSessionOnly (a human vouched — the
	// approval itself is the authority) or StandingRequired (a human delegated
	// something they already held). The SAME per-type map the runner holds
	// (Loop.PlanGateSlotStanding via PlanGateDeps.SlotStanding), so the card
	// never describes a different rule than approverCanDelegateSlots enforces.
	//
	// Vouching and delegating are different acts and the card must not render
	// them identically — see slotSentence and slotStandingSummary. Absent (nil,
	// or a type missing from it) means session-only: an unresolved status must
	// never be described as `required`, matching the runner's own bypass
	// default.
	SlotStanding map[string]string

	// SlotPermissions maps each slot's resource type to the SpiceDB permission
	// an approval on it grants (Loop.PlanGateSlotPermissions). Display only —
	// it phrases the per-slot sentence's verb and is never an input to any
	// decision. A type missing from it renders a generic, still-honest verb
	// rather than nothing.
	SlotPermissions map[string]string

	// SlotValueTransforms maps each slot's resource type to the value chain
	// that turns a declared instance value into its SpiceDB object id
	// (AgentClass.status.resolvedSlots[].valueTransforms).
	//
	// DISPLAY ONLY, and specifically for deciding when two slots naming one
	// instance are one line. git names a repository by remote URL and gh by
	// OWNER/NAME, so their raw values never match as strings even when both
	// mint the same id — rendered naively, one repository reads as two targets
	// wearing two icons. Comparing derived ids is what collapses those, and it
	// can only ever merge instances that genuinely resolve to the same object.
	//
	// A type absent from this map is compared on its raw value, which is the
	// pre-existing behavior: the dedup never merges on a guess.
	SlotValueTransforms map[string][]string
}

// BuildCard renders an approval card.
func BuildCard(in CardInput) Card {
	sev := in.Severity
	if sev == "" && in.WholePlan {
		// Priced at the WORST phase, not the first. One click here authorizes
		// every covered phase, so a plan containing an external step is an
		// external decision even when phase 1 is a read — and phase 1 being a
		// read is the common case, which is exactly how this would have rendered
		// routine.
		var all []HandleImpact
		for _, ph := range in.Plan.Phases {
			for _, h := range ph.Permissions {
				all = append(all, HandleImpact{Handle: h, StateImpact: impactOf(in.Surface, h)})
			}
		}
		sev = Classify(Request{
			Kind:              KindPlanApproval,
			HasExternalHandle: StillNeedsPerCallApproval(TierInput{Handles: all}),
			EnvelopeRecorded:  true,
		})
	}
	if sev == "" {
		sev = Routine
	}

	impact := map[permsurface.Handle]authz.StateImpact{}
	described := map[permsurface.Handle]string{}
	// Handles whose object id is fixed by the toolkit rather than supplied per
	// call — see Descriptor.ConstantResourceID.
	constants := map[permsurface.Handle]string{}
	for _, d := range in.Surface {
		impact[d.Handle] = d.StateImpact
		described[d.Handle] = d.DescribeWithTitle(in.PermissionTitles[d.ResourceType+"/"+d.Permission])
		if d.ConstantResourceID != "" {
			constants[d.Handle] = d.ConstantResourceID
		}
	}

	var phase Phase
	if in.PhaseIndex >= 0 && in.PhaseIndex < len(in.Plan.Phases) {
		phase = in.Plan.Phases[in.PhaseIndex]
	}

	lead := "Plan approval"
	if m := sev.Marker(); m != "" {
		lead = m + " " + lead
	}

	var what, why, coverage string
	var phases []CardPhase
	if in.WholePlan {
		what, phases, coverage = buildWholePlanWhat(in.Plan, impact, described, in.ResourceDisplays, constants, in.SlotValueTransforms)
		why = buildWholePlanWhy(in.Plan)
	} else {
		what = buildWhat(phase, impact, described, sev, in.MaxSingleCardHandles)
		what += buildSlotSection(withConstantInstances(phase.Slots, phase.Permissions, constants),
			in.SlotStanding, in.SlotPermissions, in.ResourceDisplays, nil)
		why = buildWhySection(phase)
	}
	if len(in.AddedHandles) > 0 || len(in.AddedSlots) > 0 {
		// An amendment is a DELTA. Restating the approved ceiling would bury the
		// lines the approver is actually deciding on.
		//
		// The structured half is DROPPED with it. What is about to describe the
		// additions alone, and phases left standing here would have a surface
		// render the full plan beside prose describing a delta — the two halves
		// contradicting each other on the one card that must not be misread.
		phases, coverage = nil, ""
		lead = "Plan amendment"
		if m := sev.Marker(); m != "" {
			lead = m + " " + lead
		}
		what = "Add to the approved plan:"
		for _, raw := range in.AddedHandles {
			h, err := permsurface.ParseHandle(raw)
			line := "\n" + describeHandle(h, raw, err == nil, described)
			// External reach is annotated PER ADDITION. On a batch card the
			// whole point is that one irreversible item among several routine
			// ones stays visible — a single annotation on the card would say
			// nothing about which addition carries it.
			if err == nil && impact[h] == authz.External {
				line += "  (leaves this session)"
			}
			what += line
		}
		// The instance axis's additions, on the SAME card as the handle ones.
		// A widening is one decision however many axes it spans — splitting it
		// would reintroduce the per-item interruption batching exists to remove.
		// The added handles carry instances too. An amendment adds a PERMISSION,
		// and the instance that permission reaches can be one the agent is
		// structurally unable to name — git_repo read/write key on the literal
		// "workspace" — so a card listing only the permission tells an approver
		// what authority is being added without telling them what it touches.
		what += buildSlotSection(
			withConstantInstances(in.AddedSlots, parseHandles(in.AddedHandles), constants),
			in.SlotStanding, in.SlotPermissions, in.ResourceDisplays,
			grantingPermissions(in.AddedHandles))
		why = in.AgentJustification
	}

	return Card{
		Lead:      lead,
		What:      what,
		Why:       sanitizeWhy(why),
		When:      cardWhen(in),
		Approvers: in.Approvers,
		Severity:  string(sev),
		// Empty on an amendment, whose What is a delta rather than a plan —
		// structure describing phases would contradict the prose beside it.
		Phases:   phases,
		Coverage: coverage,
	}
}

func impactOf(surface []permsurface.Descriptor, h permsurface.Handle) authz.StateImpact {
	for _, d := range surface {
		if d.Handle == h {
			return d.StateImpact
		}
	}
	return ""
}

// buildWholePlanWhat renders every phase of a plan-scoped approval.
//
// Phases are numbered, never named. Phase.Label is agent-authored, and this is
// the first renderer with reason to print three of them at once — doing so
// would hand a prompt-injected agent three free lines inside the half the
// approver is told to trust. The labels are still shown, attributed, in Why.
// ONE walk produces both renderings. The prose is the fallback every surface
// can print; the phases are what a surface with a layout should render instead.
// Building them separately is how the same approval ends up saying two
// different things depending on where you read it.
func buildWholePlanWhat(
	p Plan,
	impact map[permsurface.Handle]authz.StateImpact,
	described map[permsurface.Handle]string,
	resourceDisplays map[string]ResourceDisplay,
	constants map[permsurface.Handle]string,
	slotValueTransforms map[string][]string,
) (string, []CardPhase, string) {
	visible := make([]int, len(p.Phases))
	for i := range p.Phases {
		visible[i] = i
	}
	return buildProjectedPlanWhat(p, visible, impact, described, resourceDisplays, constants, slotValueTransforms)
}

// buildProjectedPlanWhat renders the What/phases/coverage for a SUBSET of a
// plan's phases — the caller's visible portion, by TRUE (0-based) plan index.
//
// A delegated child sees only its own phase(s), never the whole plan. This
// must be a first-class build over the subset, NOT a filter applied to a
// finished whole-plan card: Coverage is a whole-card conclusion computed ACROSS
// phases, so filtering a built card would name a deferred phase the child was
// deliberately not shown — the exact info leak the "its portion" rule exists to
// prevent, arriving through the one field nobody would think to filter.
//
// Phase numbers stay ABSOLUTE (Phase 3 stays "Phase 3", not renumbered to
// "Phase 1"): two cards describing the same phase must agree, and a child
// seeing "Phase 3" learns only that earlier phases exist — which an approver
// reading a grandchild's request needs anyway. Coverage's deferral count is
// driven by the VISIBLE set alone, so a later plan-wide phase's deferral cannot
// turn a child's fully-named portion into an asks-again card.
//
// buildWholePlanWhat is this with every phase visible, so the whole-plan path
// is byte-identical and there is one renderer, not two that can drift.
func buildProjectedPlanWhat(
	p Plan,
	visible []int,
	impact map[permsurface.Handle]authz.StateImpact,
	described map[permsurface.Handle]string,
	resourceDisplays map[string]ResourceDisplay,
	constants map[permsurface.Handle]string,
	slotValueTransforms map[string][]string,
) (string, []CardPhase, string) {
	if len(p.Phases) == 0 || len(visible) == 0 {
		return "No permissioned capabilities.", nil, ""
	}
	var b strings.Builder
	var deferred []int
	phases := make([]CardPhase, 0, len(visible))

	for _, i := range visible {
		if i < 0 || i >= len(p.Phases) {
			continue
		}
		ph := p.Phases[i]
		title := fmt.Sprintf("Phase %d", i+1)
		out := CardPhase{Title: title}
		fmt.Fprintf(&b, "%s:", title)
		if len(ph.Permissions) == 0 {
			b.WriteString("\n  (no permissioned capabilities)")
			out.Permissions = append(out.Permissions, CardLine{Text: "(no permissioned capabilities)"})
		}
		// Sorted by the rendered text so the prose and the structure list the
		// same ceiling in the same order.
		perms := make([]CardLine, 0, len(ph.Permissions))
		for _, h := range ph.Permissions {
			perms = append(perms, CardLine{
				Text:     describeHandle(h, h.String(), true, described),
				External: impact[h] == authz.External,
				Impact:   string(impact[h]),
				Handle:   h.String(),
			})
		}
		sort.Slice(perms, func(a, c int) bool { return perms[a].Text < perms[c].Text })
		for _, l := range perms {
			line := "  " + l.Text
			if l.External {
				line += "  (leaves this session)"
			}
			b.WriteString("\n" + line)
		}
		out.Permissions = append(out.Permissions, perms...)

		ordered := append([]Slot(nil), ph.Slots...)
		sort.Slice(ordered, func(a, c int) bool {
			if ordered[a].Type != ordered[c].Type {
				return ordered[a].Type < ordered[c].Type
			}
			return ordered[a].ID < ordered[c].ID
		})
		// One INSTANCE is one line, however many types route to it. git names a
		// repository by remote URL and gh by OWNER/NAME, so two slots of two
		// types can be the same repository — and rendered per-slot that reads
		// as two targets, one wearing a generic icon and one GitHub's. Keyed on
		// the DERIVED id, so it can only merge slots that genuinely resolve to
		// the same object; a type with no declared chain falls back to its raw
		// value and merges with nothing.
		slotSeen := make(map[string]bool, len(ordered))
		for _, s := range ordered {
			id, shown := derivedInstanceID(slotValueTransforms, s.Type, s.ID)
			if slotSeen[id] {
				continue
			}
			slotSeen[id] = true
			// The label, icon and link are all derived from the instance value
			// — the agent's own declared value, canonicalized by the type's
			// declared chain — never from anything an agent could pass off as
			// prose. resolveResourceLine falls back to s.Type (the pre-existing
			// behavior) whenever the type declared no Display at all.
			text, icon, href := resolveResourceLine(resourceDisplays, s.Type, shown)
			detail := shown
			if detail == "" {
				detail = unnamedTarget
				// An unnamed target has nothing eligible to link — href would
				// otherwise be computed from the empty string, which
				// resourcedisplay.EligibleHref already rejects, but this keeps
				// explicit rather than relying on that fallthrough.
				href = ""
			}
			line := CardLine{Text: text, Detail: detail, Icon: icon, Href: href}
			if shown != "" && s.MovedFrom != "" {
				// A MOVE, rendered exactly as the single-phase and amendment
				// cards render it (buildSlotSection): both instances, and the
				// revocation the approver must not miss. Without this a whole-plan
				// card showed a re-point as a plain first-fill while the approval
				// it recorded repointed the pin.
				displaced := displacedLabel(resourceDisplays, s.Type, s.MovedFrom)
				line.MovedFrom = displaced
				b.WriteString("\n  reaches " + text + " — " + displaced + " → " + detail)
				b.WriteString("\n    Approving moves the pin and revokes this session's access to " + displaced + ".")
			} else {
				b.WriteString("\n  reaches " + text + " — " + detail)
			}
			out.Resources = append(out.Resources, line)
		}

		// Instances the CEILING names on EVERY call, which no slot declared.
		//
		// A declared slot is the agent naming a target it chose; a constant is
		// one the toolkit fixed in advance — git.yaml keys git_repo read/write on
		// the literal "workspace", the session's own checked-out copy. Both are
		// reached, so both belong on the card. Showing only the declared slot
		// told an approver a phase reached one repository while its ceiling also
		// read and wrote a second object that appeared nowhere on the card.
		//
		// Computed, never agent-authored: the id comes from the toolkit's own
		// template resolved with no args (permsurface.Descriptor.ConstantResourceID),
		// so it sits on the trusted half of the card exactly like the permission
		// lines above it.
		constSeen := make(map[string]bool, len(ordered))
		for _, s := range ordered {
			constSeen[s.Type+":"+s.ID] = true
		}
		extras := make([]CardLine, 0, len(ph.Permissions))
		for _, h := range ph.Permissions {
			cid := constants[h]
			if cid == "" {
				continue
			}
			key := h.ResourceType() + ":" + cid
			if constSeen[key] {
				// Already named — by a slot, or by an earlier permission in this
				// same ceiling. One instance gets one line however many routes
				// arrive at it, because a human counts lines to count targets.
				continue
			}
			constSeen[key] = true
			text, icon, href := resolveResourceLine(resourceDisplays, h.ResourceType(), cid)
			extras = append(extras, CardLine{Text: text, Detail: cid, Icon: icon, Href: href})
		}
		sort.Slice(extras, func(a, c int) bool { return extras[a].Detail < extras[c].Detail })
		for _, l := range extras {
			b.WriteString("\n  reaches " + l.Text + " — " + l.Detail)
		}
		out.Resources = append(out.Resources, extras...)
		if !PhaseFullySpecified(ph) {
			deferred = append(deferred, i+1)
		}
		b.WriteString("\n")
		phases = append(phases, out)
	}

	// What the single click actually buys. Without this the approver cannot tell
	// whether saying yes ends the conversation or merely starts it — which is
	// the entire question a plan-scoped card exists to answer.
	coverage := "Approving covers every phase above."
	if len(deferred) > 0 {
		names := make([]string, 0, len(deferred))
		for _, n := range deferred {
			names = append(names, fmt.Sprintf("Phase %d", n))
		}
		coverage = "Approving covers the phases whose target is named. " +
			strings.Join(names, ", ") + " names no target yet and will ask again once it does."
	}
	coverage += "\nApproving grants only the resources you have access to; the rest go to their owners."
	b.WriteString("\n" + coverage)
	return b.String(), phases, coverage
}

// buildWholePlanWhy collects the agent's words for every phase, attributed and
// numbered so a reader can line them up against the computed half.
func buildWholePlanWhy(p Plan) string {
	parts := make([]string, 0, len(p.Phases))
	for i, ph := range p.Phases {
		label := strings.TrimSpace(ph.Label)
		why := strings.TrimSpace(ph.Why)
		switch {
		case label == "" && why == "":
			continue
		case label == "":
			parts = append(parts, fmt.Sprintf("Phase %d: %s", i+1, why))
		case why == "":
			parts = append(parts, fmt.Sprintf("Phase %d (%s)", i+1, label))
		default:
			parts = append(parts, fmt.Sprintf("Phase %d (%s): %s", i+1, label, why))
		}
	}
	return strings.Join(parts, " · ")
}

// PhaseFullySpecified reports whether every slot the phase requested names an
// instance. A phase with no slots at all is fully specified: it constrains the
// instance axis not at all, which is the shape every plan had before slots.
//
// This is what decides whether a plan-scoped approval covers a phase. A phase
// whose target is unnamed has nothing stable to key an approval to — its
// AuthorityKey covers the slot TYPE and not the instance — so pre-approving it
// would authorize a resource the human was never shown. Those phases ask again
// when they know, which is what the card says they will do.
func PhaseFullySpecified(ph Phase) bool {
	for _, s := range ph.Slots {
		if s.ID == "" {
			return false
		}
	}
	return true
}

// buildWhat renders the ceiling. Computed entirely from the frozen plan and the
// surface — it never incorporates agent text, which is what makes it safe to
// present as the authoritative description of the request.
func buildWhat(
	phase Phase,
	impact map[permsurface.Handle]authz.StateImpact,
	described map[permsurface.Handle]string,
	sev Severity,
	maxHandles int,
) string {
	if len(phase.Permissions) == 0 {
		return "No permissioned capabilities."
	}

	lines := make([]string, 0, len(phase.Permissions))
	for _, h := range phase.Permissions {
		line := describeHandle(h, h.String(), true, described)
		if impact[h] == authz.External {
			// The single most decision-relevant fact about a handle: an
			// irreversible side effect. Called out rather than left to be
			// inferred from the handle's name.
			line += "  (leaves this session)"
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)

	// Severity EXPANDS a card and never folds it. Hiding part of a ceiling
	// behind "+N more" is exactly wrong on the decisions that matter most, so
	// folding applies to Routine cards only.
	if sev == Routine && maxHandles > 0 && len(lines) > maxHandles {
		hidden := len(lines) - maxHandles
		lines = append(lines[:maxHandles:maxHandles],
			fmt.Sprintf("%s (%d)", foldMarker, hidden))
	}

	return strings.Join(lines, "\n")
}

// buildSlotSection renders the instance axis: the resource types this phase
// asks to touch, and what saying yes actually grants.
//
// Three things an approver cannot otherwise know, all load-bearing:
//
//   - A slot request is usually WHY this phase costs them anything. An
//     all-readonly ceiling auto-approves; the same ceiling with a slot request
//     does not. A card listing only handles charges them for a decision it never
//     describes — a read-only phase they are somehow being asked to approve.
//   - EACH slot's authority comes from a DIFFERENT act. A `required` slot
//     LENDS access the approver already holds; a `session-only` slot has no
//     upstream holder at all — the approval itself is the authority. These are
//     vouching and delegating, and collapsing them into one sentence would
//     misstate one of them. See slotSentence.
//   - What the SECTION as a whole grants depends on that same split: "bounded
//     by your own access" is only true when every slot in it is `required`.
//     See slotStandingSummary.
//
// standing and permission are both keyed by resource TYPE, mirroring
// CardInput.SlotStanding / SlotPermissions — passed as plain maps rather than
// folded into Slot itself, because neither is authority: standing is a
// class-resolved property re-checked fresh at approval time (see
// approverCanDelegateSlots), not something the frozen plan declares, so it
// must never enter Digest or AuthorityKey.
//
// Empty when nothing is requested: the instance axis is optional, and a card
// that mentions it when nothing asked for it trains approvers to skim past the
// section that matters when something does.
// A slot whose instance the agent could not name. Stated rather than omitted:
// an approver reading a list of named resources would otherwise take the list
// for the whole request.
const unnamedTarget = "(no target named yet)"

// displays resolves each line's human name — the SAME derivation the phase
// card uses, because an approver reading "Git repository" on one card and
// `git_repo` on the next is reading two names for one thing, and the second is
// an internal identifier that has no business on a surface a person decides
// from.
//
// granting maps a resource type to the permission THIS card actually grants on
// it, and it overrides the slot's declared one. An amendment adds a specific
// permission: a card adding `read` that says "will be able to push" attaches
// the most alarming word available to the wrong decision. Empty on a phase
// card, where the declared permission is the right answer.
func buildSlotSection(
	slots []Slot,
	standing, permission map[string]string,
	displays map[string]ResourceDisplay,
	granting map[string]string,
) string {
	if len(slots) == 0 {
		return ""
	}
	ordered := append([]Slot(nil), slots...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Type != ordered[j].Type {
			return ordered[i].Type < ordered[j].Type
		}
		return ordered[i].ID < ordered[j].ID
	})

	var b strings.Builder
	b.WriteString("\n\nAlso asks to reach:")
	unnamed := 0
	for _, s := range ordered {
		text, _, _ := resolveResourceLine(displays, s.Type, s.ID)
		b.WriteString("\n  " + text + " — ")
		// The declared value, not the object id. See the freeze site: the chain
		// that turns one into the other is injective, so this value is the only
		// one that can spend the approval.
		//
		// A MOVE (MovedFrom set on a named slot) renders both instances as
		// "<current> → <proposed>": the session is already pinned to a different
		// instance of this single-occupancy slot, and approving repoints it. A
		// first-fill (MovedFrom empty) renders the proposed instance alone,
		// byte-identically to the pre-move card.
		switch {
		case s.ID == "":
			unnamed++
			b.WriteString(unnamedTarget)
		case s.MovedFrom != "":
			b.WriteString(displacedLabel(displays, s.Type, s.MovedFrom) + " → " + s.ID)
		default:
			b.WriteString(s.ID)
		}
		perm := permission[s.Type]
		if p, ok := granting[s.Type]; ok && p != "" {
			perm = p
		}
		b.WriteString("\n    " + slotSentence(standing[s.Type], perm))
		// The revocation is the half of a move an approver must not miss: the
		// displaced instance loses this session's access when the pin moves. A
		// card naming only the new target would describe the grant and hide what
		// it costs the current one.
		if s.ID != "" && s.MovedFrom != "" {
			b.WriteString("\n    Approving moves the pin and revokes this session's access to " + displacedLabel(displays, s.Type, s.MovedFrom) + ".")
		}
	}

	// The coverage state is the actual question being answered: does saying yes
	// finish this, or is it the first of several prompts? An approver who is not
	// told cannot tell the two apart, and the whole point of naming targets in
	// the plan is that the first case becomes possible.
	if unnamed == 0 {
		b.WriteString("\nEvery resource above is named, so this approval covers them.")
	} else {
		b.WriteString("\nSome resources are not named yet, so this phase will ask again once they are.")
	}
	b.WriteString("\n" + slotStandingSummary(ordered, standing))
	return b.String()
}

// displacedLabel renders the instance a move DISPLACES — Slot.MovedFrom, which
// is a DERIVED object id read off the pin, not the raw declared value the rest
// of the slot line shows. Rendered naively, a move reads as "<escaped derived
// id> → <declared value>": two spellings of the resource in one arrow, in front
// of the approver who most needs to recognise what they are repointing.
//
// So it runs MovedFrom through the SAME display path the proposed instance uses
// (resolveResourceLine): a type that declared a Label deriver gets a per-instance
// label, recovering the readable form where the derivation happens to be
// recoverable. When no label is recoverable (resolveResourceLine can only return
// the bare type name), it falls back to the derived id itself rather than the
// type name — the approver must still see WHICH instance is being displaced, not
// merely its kind.
func displacedLabel(displays map[string]ResourceDisplay, resourceType, movedFrom string) string {
	text, _, _ := resolveResourceLine(displays, resourceType, movedFrom)
	if text == resourceType {
		return movedFrom
	}
	return text
}

// isRequiredStanding reports whether standing names the `required` mode. Any
// other value — including absence — is session-only, matching the runner's
// own default: an unresolved status must never be misread as `required`.
func isRequiredStanding(standing string) bool {
	return standing == spiceboxv1alpha1.StandingRequired
}

// slotSentence states what approving THIS slot actually does, given how its
// authority arrives. Standing is computed from the frozen plan's resolved
// config, never from anything the agent wrote — same trust level as the rest
// of What.
//
// Vouching and delegating are different acts and must not read identically: a
// `required` slot LENDS access the approver already holds, while a
// session-only slot has no upstream holder at all — the approver's decision on
// the card IS the authority. Saying the `required` sentence for a session-only
// slot would tell a human their click is bounded by their own access when, for
// that slot, it plainly is not.
func slotSentence(standing, permission string) string {
	if isRequiredStanding(standing) {
		return "You already have this access; approving lends it to this session."
	}
	if permission == "" {
		// No declared permission to name — degrade to something honest rather
		// than a broken sentence with a missing verb.
		return "This session will be able to use this for as long as it runs."
	}
	return "This session will be able to " + humanVerb(permission) + " for as long as it runs."
}

// humanVerb renders a SpiceDB permission as the verb a human reads on a card.
//
// A CLOSED map, not free-form derivation: the string reaches a human, so an
// unmapped permission degrades to its own name — honest — rather than to a
// guessed English word a card should never confidently state.
func humanVerb(permission string) string {
	switch permission {
	case "push":
		return "push"
	case "read":
		return "read"
	case "write":
		return "update"
	case "fetch":
		return "fetch"
	case "contact_access":
		return "contact"
	default:
		return permission
	}
}

// slotStandingSummary states what saying yes to this SECTION actually buys,
// given how each slot's authority arrives.
//
// The sentence this replaces unconditionally said "approving grants only the
// ones you have access to" — true for a `required` slot, and FALSE for a
// session-only one in the exact direction that matters: a session-only grant
// does not depend on the approver's access at all. Leaving the old sentence in
// place would tell a human their click is safely bounded by their own standing
// when it is not.
func slotStandingSummary(slots []Slot, standing map[string]string) string {
	allRequired, allSessionOnly := true, true
	for _, s := range slots {
		if isRequiredStanding(standing[s.Type]) {
			allSessionOnly = false
		} else {
			allRequired = false
		}
	}
	switch {
	case allRequired:
		return "Approving grants only the ones you have access to; the rest go to their owners."
	case allSessionOnly:
		return "Approving grants this session access to the named resources for as long as it runs."
	default:
		return "Approving lends this session the ones you already have access to, " +
			"and grants the rest outright — both for as long as it runs."
	}
}

// buildWhySection collects everything the AGENT wrote for this phase: its
// justification, plus its reason for each resource it named.
//
// Both belong here and nowhere else. "Which repository" is authority and is
// computed into What; "why that repository" is a claim about intent, is exactly
// the field a prompt-injected agent would use to argue for access, and so is
// rendered as the agent's words under its own heading.
//
// Joined on a separator rather than newlines because sanitizeWhy flattens
// newlines — agent text must not be able to forge card structure, and that
// includes structure inside its own section.
func buildWhySection(phase Phase) string {
	parts := make([]string, 0, 1+len(phase.Slots))
	if strings.TrimSpace(phase.Why) != "" {
		parts = append(parts, phase.Why)
	}
	for _, s := range phase.Slots {
		if strings.TrimSpace(s.Why) == "" {
			continue
		}
		// The target is TRUSTED text (validated at freeze) framing UNTRUSTED
		// text. That direction is safe; the reverse is what the split forbids.
		target := s.Type
		if s.ID != "" {
			target += " " + s.ID
		}
		parts = append(parts, target+": "+s.Why)
	}
	return strings.Join(parts, " · ")
}

// describeHandle renders one ceiling entry for a human.
//
// Falls back to the wire form when the surface never resolved the handle. That
// is the only honest option: a blank line reads as "this grants nothing", which
// is the one meaning a card must never accidentally convey, and inventing a
// description for a handle the runtime could not resolve would be worse still.
func describeHandle(h permsurface.Handle, raw string, parsed bool, described map[permsurface.Handle]string) string {
	if parsed {
		if d, ok := described[h]; ok && d != "" {
			return d
		}
	}
	return raw
}

// sanitizeWhy makes agent-authored text safe to display.
//
// It neutralizes rather than parses. Control characters and newlines are
// flattened so the text cannot forge card structure or impersonate the trusted
// What/When fields, and channel-specific control sequences are defanged.
//
// Oversized input is TRUNCATED, never dropped: a hostile agent must not be able
// to suppress its own justification by making it enormous, and an approver is
// better served by the first 2000 characters plus a visible cut than by
// silence.
func sanitizeWhy(why string) string {
	why = strings.TrimSpace(why)
	if why == "" {
		return missingWhy
	}

	var b strings.Builder
	b.Grow(len(why))
	for _, r := range why {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// Dropped entirely: control characters have no legitimate place in
			// a justification and are pure terminal/renderer attack surface.
		case r == '<' || r == '>':
			// Slack's control sequences (<!channel>, <!here>, <url|label>) and
			// any markup-shaped forgery both start here. Replacing rather than
			// escaping keeps this channel-agnostic — a kind that needs its own
			// escaping still applies it downstream.
			b.WriteRune('＜' + (r - '<')) // fullwidth < or >
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()

	if len(out) > maxWhyLen {
		cut := maxWhyLen - len(truncationMarker)
		// Do not slice mid-rune.
		for cut > 0 && !isRuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + truncationMarker
	}
	return out
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// cardWhen states the position the decision covers. A plan-scoped card is not
// at a phase, so "phase 1 of 3" would be a false statement about the thing
// being approved — it is the whole plan.
func cardWhen(in CardInput) string {
	if in.WholePlan {
		if len(in.Plan.Phases) == 1 {
			return "the whole plan (1 phase)"
		}
		return fmt.Sprintf("the whole plan (%d phases)", len(in.Plan.Phases))
	}
	return fmt.Sprintf("phase %d of %d", in.PhaseIndex+1, len(in.Plan.Phases))
}

// withConstantInstances returns slots plus any instance the given handles name
// on EVERY call, deduplicated against what is already listed.
//
// DISPLAY ONLY. The returned slice is a copy and never reaches freeze,
// PhaseFullySpecified, or the binding path: a constant is not something the
// agent declared, and treating it as a declared slot would let the ceiling
// silently widen the instance axis. It exists so the card names every object a
// phase reaches — a phase declaring git_repo:<remote URL> also reads and writes
// git_repo:workspace, and listing only the former understates the approval.
//
// Order is stable (sorted by type then id) so two builds of the same card
// produce the same prose.
func withConstantInstances(slots []Slot, perms []permsurface.Handle, constants map[permsurface.Handle]string) []Slot {
	if len(constants) == 0 || len(perms) == 0 {
		return slots
	}
	seen := make(map[string]bool, len(slots))
	for _, s := range slots {
		seen[s.Type+":"+s.ID] = true
	}
	extra := make([]Slot, 0, len(perms))
	for _, h := range perms {
		cid := constants[h]
		if cid == "" {
			continue
		}
		key := h.ResourceType() + ":" + cid
		if seen[key] {
			continue
		}
		seen[key] = true
		extra = append(extra, Slot{Type: h.ResourceType(), ID: cid})
	}
	if len(extra) == 0 {
		return slots
	}
	sort.Slice(extra, func(i, j int) bool {
		if extra[i].Type != extra[j].Type {
			return extra[i].Type < extra[j].Type
		}
		return extra[i].ID < extra[j].ID
	})
	return append(append([]Slot(nil), slots...), extra...)
}

// derivedInstanceID returns the key that decides whether two slots are one
// resource line.
//
// Two slots of DIFFERENT types merge only when they derive the same GLOBALLY
// IDENTIFYING value — in practice an absolute URL, which names one thing on
// the internet whichever type wraps it. That is the git/gh case: both mint
// https://github.com/owner/name, so one repository reached through two
// toolkits is one target.
//
// Everything else stays keyed by type as it always was. `git_repo:workspace`
// and some future `venv:workspace` derive the same STRING and are emphatically
// not the same object, and merging them would drop a target off a card an
// approver is deciding on. A type with no declared chain likewise merges with
// nothing: the dedup never merges on a guess.
// The second return is the value to DISPLAY: the canonical identity when the
// type declared a chain, else the raw value unchanged. Showing the canonical
// form is what makes the merged line honest — with two slots spelling one
// repository two ways, rendering either raw value would show a spelling the
// other tool never used, and the id the grant is actually written on is
// neither. Canonicalization only folds spelling; the host is preserved
// exactly, so a lookalike host stays as distinguishable as it ever was.
func derivedInstanceID(chains map[string][]string, resourceType, raw string) (string, string) {
	perType := resourceType + "\x00" + raw

	chain := chains[resourceType]
	if len(chain) == 0 {
		return perType, raw
	}
	ident, err := authz.ApplyTransforms(raw, identityChain(chain))
	if err != nil || ident == "" {
		return perType, raw
	}
	if u, perr := url.Parse(ident); perr != nil || u.Scheme == "" || u.Host == "" {
		return resourceType + "\x00" + ident, ident
	}
	return ident, ident
}

// identityChain drops the wire-escaping step from a value chain, leaving the
// transforms that canonicalize IDENTITY.
//
// The escape is injective, so keeping it would not change which slots compare
// equal — but it mangles a URL into `https=3A//host/...`, which no longer
// parses, and derivedInstanceID's cross-type merge turns on recognizing an
// absolute URL. Comparing the pre-escape form asks the question directly
// rather than inferring it through the escaping.
func identityChain(chain []string) []string {
	out := make([]string, 0, len(chain))
	for _, name := range chain {
		if name == "spicedb_escape" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// grantingPermissions maps each resource type an amendment touches to the
// permission it adds, read from the handles the amendment carries.
//
// Nil for a phase card: there the slot's declared permission already describes
// what approving grants, and there is no single "added" permission to prefer.
func grantingPermissions(addedHandles []string) map[string]string {
	if len(addedHandles) == 0 {
		return nil
	}
	out := make(map[string]string, len(addedHandles))
	for _, raw := range addedHandles {
		h, err := permsurface.ParseHandle(raw)
		if err != nil {
			continue
		}
		// First wins. Two handles on one type in a single amendment would make
		// "the permission being added" ambiguous, and naming one is still more
		// honest than naming the slot's declared one, which is neither.
		if _, seen := out[h.ResourceType()]; !seen {
			out[h.ResourceType()] = h.Permission()
		}
	}
	return out
}
