// Package factcontent holds the content shape the two fact Kinds share.
//
// It exists as a third package rather than living in one of them because
// neither fact Kind may import the other. The two are deliberately separate
// Kinds so that WriteAuthority — which is per-Kind — makes provenance
// structural: a runner holds a session token and therefore CANNOT author an
// envelope fact, rather than being trusted not to set a field. A shared struct
// in a neutral package keeps them symmetric without reopening that door.
//
// # Facts are keyed by the RAW identifier, and a reader must look them up that way
//
// This is the interface anything reading facts back is written against, so it
// is stated here rather than left to be inferred from the writers.
//
// Every writer files a fact under the identifier its payload actually carried:
// `demo-org/demo-repo#6`, a head commit's OID, a URL as the provider spelled
// it. Nothing applies an AuthzSlot's ValueTransforms (`spicedb_escape`,
// `normalize_url`, …) on the way in, and nothing may start: a transform chain
// is a property of the SLOT a candidate is being bound into, and one payload's
// facts can precede, outlive, or be read by more than one slot with different
// chains.
//
// So a reader holding a slot's ValueTransforms MUST look a fact up by the
// PRE-TRANSFORM value — the raw identifier — and only then transform it for
// the SpiceDB call. Looking up by the post-transform authz.ObjectID finds
// nothing, forever: `spicedb_escape` exists precisely because `#` is illegal
// in a SpiceDB object id, so a `github_pr` candidate's transformed id can
// never equal the raw key its fact was written under. That miss is silent and
// shaped exactly like a platform bug — an empty map reads as "not yet
// observed", so the gate holds closed and nothing anywhere raises an error.
// No type distinguishes the two forms today, which is why it is written down
// here.
//
// Storing raw is the deliberate choice, not an oversight: the raw identifier
// is what BOTH the envelope writer and an observes block co-derive from their
// own payload, and it is the only form in which the two can be compared to
// each other at all.
package factcontent

import (
	"crypto/sha256"
	"encoding/hex"
)

// Source records what produced a fact. Evidence for an auditor; never an input
// to any gate.
type Source struct {
	// ChannelKind and Event are set for an envelope fact.
	ChannelKind string `json:"channelKind,omitempty"`
	Event       string `json:"event,omitempty"`
	// ToolName and ToolUseID are set for an observed fact.
	ToolName  string `json:"toolName,omitempty"`
	ToolUseID string `json:"toolUseID,omitempty"`
}

// Content is one fact: a value, named, about one resource instance.
type Content struct {
	// ResourceType and ResourceID name the subject this fact is ABOUT.
	//
	// ResourceID IS THE RAW PROVIDER IDENTIFIER, PRE-TRANSFORM. It is exactly
	// what the observation derived — `demo-org/demo-repo#6` for a pull
	// request, the head OID for a commit — and no transform chain has run on
	// it. See the package doc comment for what that obliges a reader to do.
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceID"`
	// Name is the fact's name within its Kind's namespace.
	Name  string `json:"name"`
	Value any    `json:"value"`
	// ObservationID ties every entry one payload produced — all its subjects,
	// all its facts — into one group. Each entry stands alone as
	// (subject, name) -> value; this is what records that they were CO-DERIVED
	// rather than accumulated separately, and it is what an auditor follows
	// back to the single result or envelope they came from.
	ObservationID string `json:"observationID"`
	Source        Source `json:"source"`
}

// EntryID derives the memory entry id for one fact, WITHOUT the Kind's prefix
// (each Kind prepends its own).
//
// Deterministic on purpose: it is what makes two writes about the same
// (subject, name) land on the same entry, which is the write-once mechanism's
// foundation. A random id would let contradictory facts about one subject
// accumulate side by side instead of colliding.
//
// It is NOT, by itself, what makes an equal-value re-observation a no-op. Both
// Kinds are append-only, so a second Put at this id goes through the facade's
// append-only door (entriesEquivalent, pkg/memory/facade.go), which compares
// the WHOLE canonical Content — Value, ObservationID, and Source together —
// not Value alone. ObservationID and Source are per-observation by design
// (see their doc comments above), so two calls that re-derive the identical
// Value from two different tool calls or deliveries still differ at that
// layer and the facade answers ErrAppendOnlyConflict, not an idempotent
// no-op. That is fail-closed, not a hole, but it means the facade cannot be
// the layer that decides "this is the same fact, seen again."
//
// That decision belongs to the Kind's own Record function (Task 2), the same
// way pkg/memory/kinds/triggerdelivery/kind.go:66-72 documents: Record reads
// whatever already lives at the id first and compares the field that
// actually defines equality — there, the stored body; here, Content.Value
// only, and CANONICALLY rather than as Go values (sameFactValue: the stored
// side has been through JSON and the fresh side has not, so an int-valued
// fact would otherwise never compare equal to itself) — before deciding
// whether an ErrAppendOnlyConflict from the facade is a real conflict or a
// redundant re-observation to swallow. Doing that
// compare-then-tolerate at the Record layer, keyed on Value alone, is what
// lets the FIRST observation's provenance stand undisturbed while a later
// identical observation is absorbed as a no-op instead of failing loudly.
//
// The 0x1f separator is what stops field-boundary collisions in THIS id:
// without it ("ab","c") and ("a","bc") hash identically, and a caller could
// mint a fact that answers for a subject it does not name.
func EntryID(resourceType, resourceID, name string) string {
	const sep = "\x1f"
	sum := sha256.Sum256([]byte(resourceType + sep + resourceID + sep + name))
	return hex.EncodeToString(sum[:])[:32]
}
