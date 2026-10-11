// Package browserschema is the SINGLE source of truth for the SpiceDB
// substrate the browser toolbox's acting tools authorize against: the
// `web_host` resource type, its `act` permission, and the session-only slot
// shape a plan approval binds.
//
// It is the browser's equivalent of a toolkit's `spicedbSchema.resources` half
// (toolkits/git.yaml's git_repo), but a BUILT-IN rather than a user toolkit —
// the browser toolbox is not a toolkit a user installs, and a toolkit that
// declared web_host#act would fail toolkits/schema_coverage_test.go's "every
// defined permission is checked by a subcommand" sweep, because the acting
// tools are a capability (pkg/agent/tool/browser), not CLI subcommands. It
// cannot live in the base scaffold (pkg/authz/spicedb/schema/schema.zed)
// either: every definition there is RESERVED (guardian/schema.ReservedDefinitionNames),
// and ComposeSlots SKIPS a reserved type, so a reserved web_host could never
// carry the `slot_grant_act` relation a grant needs.
//
// Contributed EXACTLY ONCE as a builtin fragment (see Fragment) so the
// cluster-wide WriteSchema never carries two `definition web_host` blocks — a
// duplicate would be a hard compose conflict that wedges the schema write for
// every agent at once. Both the guardian controller (which composes the cluster
// schema) and the agentclass controller (which resolves per-class standing and
// display) read from the ONE declaration here.
package browserschema

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	// WebHostType is the SpiceDB definition name an acting tool's
	// PermissionCheck.ResourceType names, and the slot type an acting
	// AgentClass declares in spec.authz.slots. One host per instance; the id
	// is the canonical page host (browserpolicy.CanonicalHost).
	WebHostType = "web_host"

	// ActPermission is the permission an acting call checks. A plan approval
	// naming a host writes slot_grant_act (authz.SlotGrantRelationName("act")),
	// which the external-check path and the plan gate both find.
	ActPermission = "act"

	// SyntheticToolkitName names the in-memory SpiceboxToolkit the agentclass
	// controller builds from Fragment() to carry web_host into its per-class
	// standing/display resolution. Never persisted; only a browser-acting class
	// (one referencing a BrowserToolbox) gets it, so web_host's standing/display
	// never appear on an unrelated class's status. Mirrors how embedded toolkits
	// are represented as synthetic SpiceboxToolkit CRs (embeddedToolkitAsCR).
	SyntheticToolkitName = "browser"

	// icon is the generic web mark web_host instances present with on an
	// approval card. GENERIC on purpose, exactly as git_repo names "repository"
	// rather than "github": a host is host-agnostic, and claiming a vendor mark
	// would lie the moment an instance points elsewhere. Must be registered in
	// pkg/authz/plangate's knownIcons or it renders nothing.
	icon = "globe"
)

// Resource is the structured web_host definition, shaped IDENTICALLY to
// git_repo's session-only slot (toolkits/git.yaml): an inert `owner` leg plus a
// single permission whose expression the slot composer OWNS.
//
//   - standing: session-only — no local permission governs who may approve
//     reaching a web host; the session's own approvers decide and their
//     decision IS the authority (ApproverPermission must stay empty, per
//     SpiceDBResource.ValidateStanding).
//   - relation owner: user — the inert leg. Nothing ever writes a web_host
//     owner tuple, exactly as nothing writes one for a git remote URL, so the
//     `+ owner` arm ComposeSlots appends never fires; it is present only so the
//     composed `permission act = slot_grant_act->interact + owner` mirrors
//     git_repo push byte-for-byte.
//   - permission act = owner — the BASE expression. ComposeSlots REPLACES it
//     with `slot_grant_act->interact + owner` for any AgentClass that declares
//     the slot, and injects `relation slot_grant_act: agentsession with
//     expiration` alongside. Before any slot is declared the definition is
//     inert (act resolves to the empty owner set), which is correct: a type
//     nothing uses is harmless.
func Resource() spiceboxv1alpha1.SpiceDBResource {
	return spiceboxv1alpha1.SpiceDBResource{
		Name:     WebHostType,
		Standing: spiceboxv1alpha1.StandingSessionOnly,
		Relations: []spiceboxv1alpha1.SpiceDBRelation{
			{Name: "owner", SubjectType: "user"},
		},
		Permissions: []spiceboxv1alpha1.SpiceDBPermission{
			{
				Name:  ActPermission,
				Expr:  "owner",
				Title: "Act on the web page (click, type, submit)",
				PlanningNote: "Keys on the current page's host and leaves this session — every click, " +
					"keypress or form submit on a host reaches the real site. Name the host as a " +
					"web_host slot in the acting phase so a single approval covers the whole phase " +
					"rather than prompting per call.",
			},
		},
		Display: &spiceboxv1alpha1.SpiceDBResourceDisplay{
			// Name is the fallback when the deriver yields nothing; for a bare
			// host last_segment always yields the host, so this shows only for a
			// malformed id.
			Name: "Website",
			Icon: icon,
			// last_segment passes a bare canonical host ("aa.com") through
			// unchanged, so the card reads the host a human is approving rather
			// than the wire type name.
			Label: "last_segment",
		},
	}
}

// Fragment wraps Resource as the one builtin schema fragment the guardian
// controller folds into the cluster schema baseline. A new pointer per call
// (callers must not mutate the shared resource), but always the same content,
// so a byte-identical re-compose is a no-op.
func Fragment() *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{Resource()},
	}
}
