package schema

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// SlotPair is a (resourceType, permission) an AgentClass declares through
// authz.slots — the INSTANCE axis of the two-axis ceiling.
//
// It looks like GrantPair and is not one. A GrantPair puts the tuple on the
// SESSION pointing at the resource; a SlotPair puts it on the RESOURCE pointing
// at the session. That reversal is the substance: see ComposeSlots.
type SlotPair struct {
	ResourceType string
	Permission   string
}

// SlotGrantRelationName is the relation a slot writes on the resource for one
// permission. Re-exported from pkg/authz so the schema this package emits and
// the tuples that package writes are derived from ONE definition.
//
// PER-PERMISSION, and that is a security property. A slot grant is not bound to
// an exact call the way the session-grant mechanism was, so the only thing
// keeping a read-scoped slot from authorizing a write is that they are
// different relations: one shared name would make `permission read = X->…` and
// `permission write = X->…` satisfiable by the SAME tuple. Composition is
// global, so the two permissions need not even come from the same AgentClass.
// See authz.SlotGrantRelationName.
func SlotGrantRelationName(permission string) string {
	return authz.SlotGrantRelationName(permission)
}

// memoryPermissionSuffix marks a permission as belonging to the memory
// domain's audience/write pairing (view_memory, write_memory, and anything
// added later that follows the same naming). It is the PROPERTY
// isSlottableMemoryPermission keys on, not a name check — an ordinary
// permission like `read` never carries it and so is never touched by the
// guard below.
const memoryPermissionSuffix = "_memory"

// slottableMemoryPermissionPrefix is the one shape of memory permission a
// slot may name: the write side. It is deliberately a prefix (`write_...`)
// rather than the literal `write_memory`, for the same reason the suffix
// check above is a suffix and not a literal — the property, not the instance,
// is what must hold for a permission added later to inherit the guard.
const slottableMemoryPermissionPrefix = "write_"

// isSlottableMemoryPermission reports whether permission may be composed as a
// slot. It is true for anything outside the memory domain (the guard does not
// apply) and for the write side within it; false for a memory AUDIENCE
// permission such as view_memory.
//
// Why this matters: ComposeSlots OWNS the permission line it composes — see
// its doc comment — replacing whatever a fragment author wrote with
// slot_grant_<perm>->interact + owner. That is exactly the behaviour that
// makes a fragment unable to weaken who reaches a resource through an agent.
// Pointed at a memory pool's AUDIENCE permission instead of its write
// permission, the same replacement silently redefines who may read every
// entry already in that pool. Scoping the guard to `_memory`-suffixed
// permissions, rather than every permission in the schema, matters just as
// much: github_repo and github_repo_url both slot on `read` today, and a
// blanket "only write_* is slottable" rule would refuse them.
func isSlottableMemoryPermission(permission string) bool {
	if !strings.HasSuffix(permission, memoryPermissionSuffix) {
		return true
	}
	return strings.HasPrefix(permission, slottableMemoryPermissionPrefix)
}

// IsMemoryAudiencePermission reports whether permission is a memory pool's
// AUDIENCE permission (view_memory-shaped) rather than an ordinary one — the
// exact negation of isSlottableMemoryPermission, exported under its own name
// because the property has a second consumer outside this package:
// toolkits/schema_coverage_test.go's "every defined permission is checked by
// some subcommand" sweep. An audience permission is never named by a
// subcommand's own PermissionCheck — it is expanded live, by LookupSubjects,
// wherever the runner needs a pool's readership — so treating it like an
// ordinary permission there would flag every toolkit that ships one
// (github_pull_request#view_memory today) as dead schema.
func IsMemoryAudiencePermission(permission string) bool {
	return !isSlottableMemoryPermission(permission)
}

// slotGrantRelationDecl carries `with expiration`, which makes an expiry
// MANDATORY on every slot-grant tuple SpiceDB will accept.
//
// That is the point. Teardown alone cannot be the only bound on a slot grant:
// the grant lives on somebody else's RESOURCE, the AgentSession CR is retained
// after completion, and the grant keeps resolving — so a teardown that fails
// once leaks live authority indefinitely. Requiring the expiry in the SCHEMA
// means a writer cannot forget it; an "indefinite" tuple stops being
// expressible rather than merely discouraged.
func slotGrantRelationDecl(permission string) string {
	return SlotGrantRelationName(permission) + ": agentsession with expiration"
}

// slotPermissionExpr is the expression the composer OWNS for a slot permission.
//
//	slot_grant_<perm>->interact   the session's member set (owner + participant - denied)
//	owner                         direct standing on the resource itself, WHEN the
//	                              target definition declares one
//
// The second leg is why approval volume stays sane: a requester who already has
// standing on the resource passes with no grant and no approval, so approval
// fires only where the agent would exceed what the requester could do alone.
//
// hasOwner is false for a target definition that declares no `owner` relation
// or permission — a session-only resource (SpiceDBResource.Standing) is
// exactly this shape, since its ValidateStanding actively forbids naming an
// approver permission at all. Such a definition has no local standing for the
// leg to mean anything, so omitting it NARROWS what the composed permission
// grants rather than widening it: the fail-closed direction, not fail-open.
// Emitting it unconditionally instead produces `owner` resolving to nothing,
// which is schema SpiceDB's type system refuses at the whole-cluster
// WriteSchema — see definitionDeclaresName, which composeOneSlot calls to
// decide hasOwner before this function ever runs.
func slotPermissionExpr(permission string, hasOwner bool) string {
	expr := SlotGrantRelationName(permission) + "->interact"
	if hasOwner {
		expr += " + owner"
	}
	return expr
}

// ComposeSlots injects each slot's grant relation and permission arm into the
// resource's definition block, returning the new schema and whether anything
// changed.
//
// The composer OWNS the permission line rather than unioning into one the
// fragment author wrote. That is a deliberate trade: an author loses the ability
// to write that single permission, and in exchange the expression that decides
// who may reach the resource through an agent cannot be weakened by a fragment,
// nor silently omitted. A fragment that still carries a wildcard leaf in this
// position is therefore TIGHTENED by composition, not honored — which is the
// point, since the only reason such a leaf was ever structurally necessary was
// the session-grant tuple direction that slots replaced.
//
// A slot naming a definition the schema does not declare is skipped rather than
// erroring: composition is global and all-or-nothing, so one AgentClass naming a
// type whose MCPServer was deleted must not wedge schema writes for every other
// agent in the cluster.
// Returns the slots it SKIPPED alongside the composed text, so a caller can
// report them rather than leaving a declared slot silently inert.
//
// A slot naming a memory audience permission — one ending `_memory` without a
// `write_` prefix — is refused with an ERROR rather than skipped or composed,
// because a slot there would silently replace the audience definition. See
// isSlottableMemoryPermission.
func ComposeSlots(src string, slots []SlotPair) (string, bool, []SlotPair, error) {
	out := src
	changed := false
	var skipped []SlotPair
	for _, s := range slots {
		if s.ResourceType == "" || s.Permission == "" {
			continue
		}
		// A memory AUDIENCE permission (view_memory, and anything later paired
		// with it) may never be slotted: this composer OWNS the permission line
		// it writes, so a slot on view_memory would silently replace "who may
		// read this pool" with the slot's interact+owner expression, changing
		// every entry's readership out from under the data with no error and no
		// diff anyone reads. Unlike a reserved-definition slot (skipped, below),
		// this is refused with an error: the operator asked for something that
		// would have corrupted a pool's audience, and needs to be told, not to
		// have it quietly dropped.
		if !isSlottableMemoryPermission(s.Permission) {
			return "", false, nil, fmt.Errorf(
				"slot %s/%s: refused: %q is a memory audience permission, not a write_* memory permission, and is not slottable — "+
					"a slot composes over the permission it names, so granting one on the audience permission would silently "+
					"change who may read the pool; only a write_* memory permission may be granted as a slot",
				s.ResourceType, s.Permission, s.Permission)
		}
		// A slot may not rewrite a definition the SCAFFOLD owns, whatever an
		// AgentClass declares. This is the same derived set the fragment path
		// refuses (ReservedDefinitionNames, compiled from schema.zed), not a
		// hand-kept subset of it: the two guard the same thing from opposite
		// directions, and the short list here silently permitted a slot to
		// overwrite memory_entry#read, artifact#view, pt_tag#reader and
		// agentclass#start_session with the slot form.
		//
		// Skipped-and-reported rather than an error, matching the treatment of a
		// slot naming an UNDECLARED type a few lines down: composition is global
		// and all-or-nothing, so one AgentClass must not wedge schema writes for
		// every other agent in the cluster.
		if _, reserved := ReservedDefinitionNames()[s.ResourceType]; reserved {
			skipped = append(skipped, s)
			continue
		}
		next, did, skip, err := composeOneSlot(out, s)
		if err != nil {
			return "", false, nil, err
		}
		if skip {
			skipped = append(skipped, s)
		}
		if did {
			changed = true
		}
		out = next
	}
	return out, changed, skipped, nil
}

// composeOneSlot rewrites a single definition block. The bool results are
// (changed, skipped).
func composeOneSlot(src string, s SlotPair) (string, bool, bool, error) {
	start, end, ok := definitionBlockBounds(src, s.ResourceType)
	if !ok {
		return src, false, true, nil // type not declared here; skipped by design
	}
	block := src[start:end]

	// A grant_* leg here used to mean "still on the session-grant path, leave it
	// alone": composing over it would have deleted the leg the live approval flow
	// wrote and checked. That flow is retired — nothing writes a grant_* tuple
	// any more — so such a leg is now a dead relation, and composing over it is
	// the correct outcome rather than a destructive one. The guard that skipped
	// these resources came out with the mechanism it protected.

	wantRel := "    relation " + slotGrantRelationDecl(s.Permission)

	// The pin relation is per resource TYPE, not per permission: every
	// permission of a single-occupancy slot shares one pin. Emitted for every
	// slot resource (multi included) because its presence enforces nothing —
	// enforcement is entirely write-time — and composing it unconditionally
	// keeps occupancy out of the cross-class slot-pair union.
	wantPin := "    relation " + authz.SlotPinRelationName + ": agentsession"

	var kept []string
	hasRel, hasPerm, hasPin := false, false, false
	permLine := regexp.MustCompile(`^\s*permission\s+` + regexp.QuoteMeta(s.Permission) + `\s*=`)

	// The composer is about to unconditionally overwrite the line permLine
	// matches (below), whatever the fragment author put there — that includes
	// text that isn't even valid SpiceDB syntax standalone (a bare `user:*`
	// wildcard leaf is not legal outside a relation's subject-type list; see
	// TestComposeSlots_noWildcardLeafSurvives). definitionDeclaresName compiles
	// its input, so probing it against block AS WRITTEN would let exactly the
	// content this composer exists to discard break the owner check that
	// decides how to replace it. Strip that one line first — every OTHER
	// member of the definition, which the probe cares about, is untouched by
	// this call to begin with.
	var probeLines []string
	for _, line := range strings.Split(block, "\n") {
		if !permLine.MatchString(line) {
			probeLines = append(probeLines, line)
		}
	}
	// Checked against the target's OWN (probed) text, not the composed `want`
	// line this call is about to build: whether the resource declares `owner`
	// is a property of what the fragment author wrote, not of anything this
	// composer is adding.
	hasOwner, err := definitionDeclaresName(strings.Join(probeLines, "\n"), s.ResourceType, "owner")
	if err != nil {
		return src, false, false, fmt.Errorf("slot %s/%s: check for owner relation: %w", s.ResourceType, s.Permission, err)
	}

	want := "    permission " + s.Permission + " = " + slotPermissionExpr(s.Permission, hasOwner)

	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == strings.TrimSpace(wantRel):
			hasRel = true
			kept = append(kept, wantRel)
		case trimmed == strings.TrimSpace(wantPin):
			hasPin = true
			kept = append(kept, wantPin)
		case permLine.MatchString(line):
			// The composer owns this line: whatever the author wrote is
			// replaced, which is what removes a wildcard leaf.
			hasPerm = true
			kept = append(kept, want)
		default:
			kept = append(kept, line)
		}
	}

	var toInsertAfterHeader []string
	if !hasRel {
		toInsertAfterHeader = append(toInsertAfterHeader, wantRel)
	}
	if !hasPin {
		toInsertAfterHeader = append(toInsertAfterHeader, wantPin)
	}
	if len(toInsertAfterHeader) > 0 {
		kept = insertAfterDefinitionHeader(kept, toInsertAfterHeader...)
	}
	if !hasPerm {
		// BEFORE the closing brace. Appending to the block's lines put the
		// permission AFTER the "}", producing schema SpiceDB cannot parse. Every
		// fixture happened to declare its permission already, so the replace
		// path ran and this was never exercised — but the composer creating the
		// line is a normal case, not an exotic one.
		kept = insertBeforeDefinitionClose(kept, want)
	}

	rebuilt := strings.Join(kept, "\n")
	if rebuilt == block {
		return src, false, false, nil
	}
	return src[:start] + rebuilt + src[end:], true, false, nil
}

// insertAfterDefinitionHeader places one or more relations immediately after
// the `definition X {` line, so relations stay grouped above permissions the
// way a hand-written schema reads. Variadic so a caller inserting more than
// one missing relation in the same pass (the grant relation and the pin) gets
// them placed together, in the order given, rather than one push reordering
// the other.
func insertAfterDefinitionHeader(lines []string, relations ...string) []string {
	for i, l := range lines {
		if strings.HasSuffix(strings.TrimSpace(l), "{") {
			out := make([]string, 0, len(lines)+len(relations))
			out = append(out, lines[:i+1]...)
			out = append(out, relations...)
			return append(out, lines[i+1:]...)
		}
	}
	return append(lines, relations...)
}

// definitionBlockBounds returns the [start,end) offsets of `definition <name> {
// ... }` in src, brace-matched so a nested brace cannot end the block early.
func definitionBlockBounds(src, name string) (int, int, bool) {
	re := regexp.MustCompile(`(?m)^definition\s+` + regexp.QuoteMeta(name) + `\s*\{`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		return 0, 0, false
	}
	depth := 0
	for i := loc[1] - 1; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return loc[0], i + 1, true
			}
		}
	}
	return 0, 0, false
}

// SlotPairsFromStrings is a convenience for callers holding parallel slices.
func SlotPairsFromStrings(resourceTypes, permissions []string) ([]SlotPair, error) {
	if len(resourceTypes) != len(permissions) {
		return nil, fmt.Errorf("schema: %d resource types but %d permissions", len(resourceTypes), len(permissions))
	}
	out := make([]SlotPair, 0, len(resourceTypes))
	for i := range resourceTypes {
		out = append(out, SlotPair{ResourceType: resourceTypes[i], Permission: permissions[i]})
	}
	return out, nil
}

// insertBeforeDefinitionClose places a line immediately before the block's
// final closing brace, so a permission the composer creates lands INSIDE the
// definition. Falls back to appending when no closing brace is found, which
// cannot happen for a brace-matched block.
func insertBeforeDefinitionClose(lines []string, line string) []string {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "}" {
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:i]...)
			out = append(out, line)
			return append(out, lines[i:]...)
		}
	}
	return append(lines, line)
}
