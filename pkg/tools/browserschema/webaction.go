package browserschema

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// web_action is the REVALIDATION sibling of web_host (schema.go). web_host is
// the HOST-level standing slot: one approval covers acting on a whole host for
// the acting phase. web_action is the PER-ACTION gate on top of it, and the one
// distinguishing fact is the caveat its slot grant carries. The guardian
// composer emits web_action's `slot_grant_act` relation as `with
// pagestate_matches and expiration`, SCOPED to web_action alone (see
// pkg/authz/guardian/schema/slots.go's slotGrantRelationDecl) — so a grant on
// web_action#act resolves only while the page's deterministic fingerprint still
// matches the one captured at approval (Phase E's TOCTOU gate). web_host#act
// carries no such caveat: a host-level grant has no page fingerprint to bind to.
//
// The two definitions are otherwise shaped IDENTICALLY — an inert owner leg plus
// an `act` permission the slot composer owns — and they REUSE the same
// ActPermission, so both derive the same slot-grant relation NAME
// (slot_grant_act). That is deliberate: the caveat, not the relation name, is
// what separates a revalidated action grant from a standing host grant, which
// is why the composer keys the caveat on the resource TYPE and not on the
// permission.
const (
	// WebActionType is the SpiceDB definition name a per-action revalidation
	// check names (PermissionCheck.ResourceType), and the slot type a browser
	// acting AgentClass declares in spec.authz.slots alongside — or instead of —
	// web_host. Its id is the escaped ActionKey (host + op + element facts), and
	// on a canonical lower-cased host it escapes to the same host prefix a
	// web_host id carries — see WebActionValueTransforms for why this chain is
	// spicedb_escape alone while web_host's now casefolds first.
	WebActionType = "web_action"

	// actionIcon is the mark a web_action instance presents with on a
	// revalidation approval card. GENERIC on purpose, exactly as web_host's is
	// and as git_repo names "repository" rather than "github": an action is
	// host-agnostic. Must be registered in pkg/authz/plangate's knownIcons or it
	// renders nothing. Shares web_host's "globe" — both are the browser surface.
	actionIcon = icon

	// WebActionKeyArg, WebActionFingerprintArg and WebActionRequiredArg are the
	// NamedArgs keys the browser acting tool STAMPS for a call it classified as a
	// mutate (Phase E's E8 gate), and the dispatch layer (pkg/agent/runner's
	// BuildInputs) reads back off the normalized arg map to lift into typed
	// authz.Inputs fields. They are the single shared contract between the two
	// halves — what the tool writes and what dispatch reads — kept here in
	// browserschema alongside WebActionType/WebActionValueTransforms so the
	// web_action gate's whole vocabulary lives in one place and the producer and
	// consumer cannot drift apart.
	//
	//   - WebActionKeyArg carries the raw, un-escaped web_action key (E4 ActionKey);
	//     dispatch escapes it with WebActionValueTransforms into the SpiceDB object
	//     id the per-action grant was written on.
	//   - WebActionFingerprintArg carries the live page fingerprint (E5
	//     PageFingerprint) presented as the pagestate_matches caveat's check-time
	//     context.
	//   - WebActionRequiredArg is the mutate flag: present+true tells dispatch this
	//     call needs the SECOND (web_action) check on top of the coarse web_host one.
	//     observe/navigate/session calls stamp none of these.
	WebActionKeyArg         = "web_action_key"
	WebActionFingerprintArg = "web_action_fingerprint"
	WebActionRequiredArg    = "web_action_required"

	// WebActionDescriptionArg is a FOURTH NamedArgs key, stamped alongside (but
	// independently of) the three above: a short, human-readable sentence
	// describing what the pending acting call will do — "Click 'Submit order'",
	// "Select 'Medium'" — derived from the SAME action facts the consequence
	// judge classifies, not from the wire-level op/ref. Unlike the other three,
	// it is stamped for EVERY acting call the tool can read facts for (mutate or
	// not), because even a benign click can need the coarse web_host approval
	// (e.g. the agent's first action on a new host) and that card deserves the
	// same human phrasing. The runner's approval-ask builder reads this key and,
	// when present, uses it as the card's "What" line INSTEAD OF the generic
	// tool-call summarizer — then deletes it from the args map before the card's
	// argsHash/ArgsJSON are computed, exactly like the three keys above, so this
	// presentation string never leaks into the durable args hash.
	WebActionDescriptionArg = "web_action_description"

	// PagestateMatchesCaveat is the SpiceDB caveat name the composer emits on
	// web_action's slot_grant_act relation (`with pagestate_matches and
	// expiration`) — defined in the canonical scaffold schema (E1,
	// pkg/authz/spicedb/schema/schema.zed). The E9 approval-write path pins it as
	// SlotBinding.CaveatName when minting an approved web_action grant, and the E8
	// dispatch check (pkg/authz/spicedb/toolcheck) presents this caveat's live
	// context. Kept here as the single source of truth so the write side and the
	// check side cannot name two different caveats.
	PagestateMatchesCaveat = "pagestate_matches"

	// ApprovedFingerprintParam is the pagestate_matches caveat parameter the
	// GRANT freezes at approval time — the fingerprint of the page the human saw.
	// The check presents its sibling parameter (presented_fingerprint, the live
	// page) and the caveat resolves only while `presented_fingerprint != "" &&
	// approved_fingerprint == presented_fingerprint`. So an approved grant whose
	// approved_fingerprint is "" is permanently dead (nothing a live page presents
	// can equal "" past the guard) — which is why the E9 write refuses to mint one
	// rather than cache an approval against a page it could not fingerprint.
	ApprovedFingerprintParam = "approved_fingerprint"
)

// WebActionValueTransforms is the transform chain that turns a web_action
// ActionKey (the raw, un-escaped E4 key — host + op + element facts) into its
// SpiceDB web_action object id, and the SINGLE source of truth for that chain.
//
// It is kept as its own function rather than aliasing WebHostValueTransforms so
// the two can diverge without a silent coupling — and they now DO, deliberately:
// web_host casefolds before escaping (WebHostValueTransforms), this does not.
// The asymmetry is correct because the two slots differ in where their keys come
// from. A web_host id is derived by TWO independent legs that disagree on case —
// the check uses the already-lower-cased Session.CurrentHost(), the plan-gate
// grant write uses the free-form host a planner typed — so web_host must fold
// case to make them meet. A web_action id has NO such split: both the grant
// write (persistWebActionApproval) and the E8 check (toolcheck) key off the SAME
// tool-stamped ActionKey, whose host segment is already CanonicalHost()'s
// lower-cased form, so there is no case mismatch to fold out — and casefolding
// here would needlessly lowercase the non-host ActionKey segments too. spicedb_escape
// is INJECTIVE (pkg/authz/transforms.go), so distinct keys never collapse onto
// one id and a revalidation card stays a truthful claim about exactly which
// action was approved. A fresh slice per call so no caller can mutate a shared one.
func WebActionValueTransforms() []string {
	return []string{"spicedb_escape"}
}

// WebActionResource is the structured web_action definition, shaped IDENTICALLY
// to web_host's session-only slot (see Resource in schema.go): an inert `owner`
// leg plus a single `act` permission whose expression the slot composer OWNS.
//
//   - standing: session-only — the per-action approval IS the authority; no
//     local permission governs who may approve acting on a page
//     (ApproverPermission must stay empty, per SpiceDBResource.ValidateStanding).
//   - relation owner: user — the inert leg. Nothing ever writes a web_action
//     owner tuple, exactly as nothing writes one for web_host, so the `+ owner`
//     arm ComposeSlots appends never fires; it is present only so the composed
//     `permission act = slot_grant_act->interact + owner` mirrors web_host
//     byte-for-byte.
//   - permission act = owner — the BASE expression. ComposeSlots REPLACES it
//     with `slot_grant_act->interact + owner` for any AgentClass that declares
//     the slot, and injects `relation slot_grant_act: agentsession with
//     pagestate_matches and expiration` alongside — the pagestate_matches caveat
//     being the one thing that distinguishes this from web_host. Before any slot
//     is declared the definition is inert (act resolves to the empty owner set),
//     which is correct: a type nothing uses is harmless.
func WebActionResource() spiceboxv1alpha1.SpiceDBResource {
	return spiceboxv1alpha1.SpiceDBResource{
		Name:     WebActionType,
		Standing: spiceboxv1alpha1.StandingSessionOnly,
		Relations: []spiceboxv1alpha1.SpiceDBRelation{
			{Name: "owner", SubjectType: "user"},
		},
		Permissions: []spiceboxv1alpha1.SpiceDBPermission{
			{
				Name:  ActPermission,
				Expr:  "owner",
				Title: "Take this web action (revalidated against the live page)",
				PlanningNote: "Gates a SINGLE browser action and is revalidated against the page's " +
					"deterministic fingerprint at call time — the approval only holds while the page " +
					"still looks the way it did when approved. Do NOT name web_action as a plan slot: " +
					"this per-action gate is minted automatically at call time and re-approved against " +
					"live page state on each click, keypress or form submit, so a plan phase cannot " +
					"grant it ahead of time (and a phase that named it would bind nothing). To let the " +
					"agent act on a site, name the HOST as a web_host slot in the acting phase — the " +
					"coarse host-level grant — and the per-action web_action re-check layers on top of " +
					"it automatically.",
			},
		},
		Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{
			// Name is the fallback when the deriver yields nothing; for a bare
			// host last_segment always yields the host, so this shows only for a
			// malformed id.
			Name: "Web action",
			Icon: actionIcon,
			// last_segment passes a bare canonical host ("aa.com") through
			// unchanged, so the card reads the host a human is approving rather
			// than the wire type name.
			Label: "last_segment",
		},
	}
}

// WebActionFragment wraps WebActionResource as a builtin schema fragment, the
// web_action counterpart to web_host's Fragment. Contributed EXACTLY ONCE
// cluster-wide (the guardian controller's schemaBaseline) so the composed
// schema never carries two `definition web_action` blocks — a duplicate would
// be a hard compose conflict that wedges the schema write for every agent at
// once. A new pointer per call (callers must not mutate the shared resource),
// but always the same content, so a byte-identical re-compose is a no-op.
func WebActionFragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{WebActionResource()},
	}
}
