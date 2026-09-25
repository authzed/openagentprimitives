// Package provenance builds and validates pt-tags: the per-datum record of
// where a piece of information came from and who may see it.
//
// The audience arithmetic itself lives in SpiceDB (`definition pt_tag` in
// pkg/authz/spicedb/schema/schema.zed), where confidentiality is an
// intersection (`derived_from.all(reader)`) and integrity a union
// (`derived_from->carries_untrusted`). This package's job is the one rule the
// schema CANNOT state: that a tag is a leaf or derived and never both.
//
// That rule is load-bearing. `reader = direct_reader + derived_from.all(reader)`
// unions its two arms, so a derived tag carrying its own direct readers is
// readable by anyone in that set REGARDLESS of the intersection below it —
// which is precisely the declassification the lattice exists to prevent, and
// it would be invisible: the schema compiles, every check answers, and the
// answers are simply wider than they should be.
package provenance

import "fmt"

// TagID identifies one pt-tag. It is the SpiceDB object id of a `pt_tag`.
type TagID string

// kind distinguishes a leaf from a derived tag. It is stored rather than
// inferred from which slice is populated, so that a leaf whose source has NO
// authorized readers is still unambiguously a leaf — inference would read that
// legitimate case as the zero value.
type kind uint8

const (
	kindUnset kind = iota
	kindLeaf
	kindDerived
)

// Tag is one datum's provenance. Its fields are unexported and it is only
// obtainable from NewLeaf or NewDerived, so the leaf-or-derived invariant is
// enforced by the type's API rather than by a validator someone can forget to
// call: there is no exported way to express a tag that is both.
//
// The zero Tag is deliberately inert — neither leaf nor derived — so one
// arriving from a struct field or a map miss cannot pass as an empty-reader
// leaf, which would be an undisclosable datum masquerading as a checked one.
type Tag struct {
	id              TagID
	kind            kind
	derivedFrom     []TagID
	directReaders   []string
	untrustedOrigin bool
}

// NewLeaf builds a tag for a datum that entered the session directly, with the
// audience authorized on the resource it came from.
//
// readers may be empty: LookupSubjects over a resource nobody can read
// legitimately returns nothing, and the resulting tag is readable by nobody,
// which is the correct fail-closed answer. Rejecting it would push the caller
// into dropping the datum untracked or inventing an audience.
//
// untrustedOrigin marks data from a source that may carry injected content. It
// is a LEAF property: integrity is declared where a datum enters and inherited
// everywhere below it, via carries_untrusted's arrow.
func NewLeaf(id TagID, readers []string, untrustedOrigin bool) (Tag, error) {
	if id == "" {
		return Tag{}, fmt.Errorf("provenance: a leaf tag needs an id")
	}
	return Tag{
		id:              id,
		kind:            kindLeaf,
		directReaders:   append([]string(nil), readers...),
		untrustedOrigin: untrustedOrigin,
	}, nil
}

// NewDerived builds a tag for a datum assembled from others. Its audience is
// the INTERSECTION of its sources' audiences, resolved in SpiceDB rather than
// computed here — the reader set is live, so revoking access to one source
// narrows every tag derived from it at the next check, with no invalidation
// pass.
//
// It takes no readers. That is the invariant, expressed as a signature rather
// than as a validation: there is no argument through which a caller could
// widen a derived tag, so no code path has to remember to refuse one.
//
// It takes no untrustedOrigin either. carries_untrusted resolves through
// derived_from, so a derived tag inherits taint from its sources; letting one
// declare itself clean would be laundering in the integrity direction.
//
// At least one source is required. A tag derived from nothing is readable by
// nobody — safe, but it is a caller bug rather than a fact about any data, and
// it would look like a deliberate undisclosable datum.
func NewDerived(id TagID, sources []TagID) (Tag, error) {
	if id == "" {
		return Tag{}, fmt.Errorf("provenance: a derived tag needs an id")
	}
	if len(sources) == 0 {
		return Tag{}, fmt.Errorf("provenance: derived tag %q needs at least one source", id)
	}
	return Tag{
		id:          id,
		kind:        kindDerived,
		derivedFrom: append([]TagID(nil), sources...),
	}, nil
}

// ID returns the tag's identifier.
func (t Tag) ID() TagID { return t.id }

// IsLeaf reports whether this tag names its own audience.
func (t Tag) IsLeaf() bool { return t.kind == kindLeaf }

// IsDerived reports whether this tag's audience comes from its sources.
func (t Tag) IsDerived() bool { return t.kind == kindDerived }

// UntrustedOrigin reports the leaf's own integrity mark. Always false for a
// derived tag, which inherits taint through derived_from instead.
func (t Tag) UntrustedOrigin() bool { return t.untrustedOrigin }

// DirectReaders returns a copy of the leaf's audience; empty for a derived
// tag. Copied because an authorization input a consumer can edit in place is
// not an input.
func (t Tag) DirectReaders() []string {
	return append([]string(nil), t.directReaders...)
}

// DerivedFrom returns a copy of the tag's sources; empty for a leaf. Copied
// for the same reason as DirectReaders — the derivation tree decides the
// audience, so a caller must not be able to rewrite it after the fact.
func (t Tag) DerivedFrom() []TagID {
	return append([]TagID(nil), t.derivedFrom...)
}
