// Package pttag is the memory Kind for per-datum provenance tags.
//
// Each entry is the durable form of one provenance.Tag: where a datum came
// from, what it was derived from, and who may see it. Together they are the
// derivation forest the egress check resolves a payload's audience through.
//
// Distinct from infoleakage_taint, which records one resource read per tool
// call and is answered SESSION-WIDE. These are per datum, and they remain
// unwritten unless the fine_grained_info_leakage capability is granted.
package pttag

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const KindName = "pt_tag"

// Tag kinds as stored. Spelled out rather than derived from a bool so a third
// shape (should one ever exist) is an unknown value that ToTag refuses, not a
// silent reinterpretation of an existing record.
const (
	KindLeaf    = "leaf"
	KindDerived = "derived"
)

// TagRecord is the durable form of one provenance tag.
//
// A separate type from provenance.Tag on purpose: provenance.Tag keeps its
// fields unexported so the leaf-or-derived invariant cannot be bypassed, which
// also means it cannot be a serialization target. This is the wire shape, and
// ToTag is the only way back — so a record that was corrupted, hand-edited, or
// written by an older version is re-validated on READ rather than trusted
// because it is already stored.
type TagRecord struct {
	// ID is the tag's identifier and the SpiceDB `pt_tag` object id.
	ID string `json:"id"`
	// Kind is KindLeaf or KindDerived.
	Kind string `json:"kind"`
	// DerivedFrom names this tag's sources; empty for a leaf.
	DerivedFrom []string `json:"derivedFrom,omitempty"`
	// DirectReaders is the leaf's audience; empty for a derived tag, whose
	// audience is the intersection resolved through DerivedFrom in SpiceDB.
	DirectReaders []string `json:"directReaders,omitempty"`
	// UntrustedOrigin marks a leaf whose source may carry injected content.
	// Always false on a derived record: carries_untrusted resolves through
	// derived_from, so a derived tag inherits taint rather than declaring it.
	UntrustedOrigin bool `json:"untrustedOrigin,omitempty"`
	// Sources names the SpiceDB objects this leaf's audience was derived from,
	// as "type:id". Empty for a derived tag, whose sources are DerivedFrom.
	//
	// Recorded because the reader set alone says WHO may see the datum and
	// never WHY. Two things need the why: an audit reconstructing how an
	// audience was arrived at, and any routing that must find a human able to
	// speak for the datum — `pt_tag` itself has no owner relation, so the only
	// route to one is back through the objects it came from.
	Sources []string `json:"sources,omitempty"`

	// ToolUseID is the tool_use block whose result carried this datum into
	// context, mirroring infoleakage_taint's link of the same name.
	ToolUseID string `json:"toolUseID,omitempty"`
	// MintedAt is when the tag was created; used as the entry's CreatedAt.
	MintedAt time.Time `json:"mintedAt"`
}

// FromTag renders a tag into its durable form.
//
// sources are the "type:id" objects a LEAF's audience was derived from; the
// caller passes nil for a derived tag, whose sources are its DerivedFrom.
func FromTag(t provenance.Tag, sources []string, toolUseID string, mintedAt time.Time) TagRecord {
	rec := TagRecord{
		ID:              string(t.ID()),
		Sources:         sources,
		DirectReaders:   t.DirectReaders(),
		UntrustedOrigin: t.UntrustedOrigin(),
		ToolUseID:       toolUseID,
		MintedAt:        mintedAt,
	}
	for _, s := range t.DerivedFrom() {
		rec.DerivedFrom = append(rec.DerivedFrom, string(s))
	}
	switch {
	case t.IsLeaf():
		rec.Kind = KindLeaf
	case t.IsDerived():
		rec.Kind = KindDerived
	}
	return rec
}

// ToTag rebuilds a tag from its durable form, THROUGH the constructors.
//
// It deliberately does not populate a struct directly. A stored record is
// data, and data can be wrong — truncated, hand-edited, or written by a build
// that predates a rule. Routing it through NewLeaf/NewDerived means the
// leaf-or-derived invariant is re-checked at the read boundary, so a record
// carrying both direct readers and derivation edges is REFUSED rather than
// loaded into a tag that would be read through both arms of `reader` and
// resolve wider than any of its sources allow.
func (r TagRecord) ToTag() (provenance.Tag, error) {
	switch r.Kind {
	case KindLeaf:
		if len(r.DerivedFrom) > 0 {
			return provenance.Tag{}, fmt.Errorf("pttag: record %q is a leaf but carries %d derivation edge(s); refusing to load a tag that would be read through both arms of `reader`", r.ID, len(r.DerivedFrom))
		}
		return provenance.NewLeaf(provenance.TagID(r.ID), r.DirectReaders, r.UntrustedOrigin)
	case KindDerived:
		if len(r.DirectReaders) > 0 {
			return provenance.Tag{}, fmt.Errorf("pttag: record %q is derived but carries %d direct reader(s); that widens past the intersection its sources define", r.ID, len(r.DirectReaders))
		}
		sources := make([]provenance.TagID, 0, len(r.DerivedFrom))
		for _, s := range r.DerivedFrom {
			sources = append(sources, provenance.TagID(s))
		}
		return provenance.NewDerived(provenance.TagID(r.ID), sources)
	default:
		return provenance.Tag{}, fmt.Errorf("pttag: record %q has unknown kind %q; refusing rather than guessing an audience", r.ID, r.Kind)
	}
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "ptt-" }

// WriteAuthority is COMPONENT-written, and deliberately NOT session-written
// like the infoleakage_taint records it sits beside. The two look alike and
// their threat models are opposites.
//
// A taint record RESTRICTS: more taint means a narrower audience. A session
// that wanted to cheat would OMIT one, and an omission leaves the payload not
// fully accounted for, which degrades to the coarse session-wide check — it
// over-blocks. The failure direction is safe, which is what makes
// infoleakage_taint tolerable as a session-written kind.
//
// A pt_tag GRANTS: a leaf's direct_reader set is the audience allowed to see
// the datum, so a WIDER set permits more disclosure. A session that could
// author its own tags could mint one naming an audience its source never
// authorized, and then legitimately disclose to them — the tag would be
// checked, and it would say yes. That is a laundering primitive at the storage
// layer, handing the pen for a permission to the party it constrains.
//
// WriteAuthority's own doc draws exactly this line: "If a component reads the
// Kind to make an authorization decision, it is not [SessionWritten]." The
// egress check reads these tags to decide an audience, so this is that case.
//
// Consequence for minting: the runner may TRIGGER a mint on its tool-call
// path, but the audience must be derived by the component that writes the
// record — LookupSubjects performed by the writer, never a reader set the
// runner supplies and the component records on trust.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn: []memory.SignalKind{lifecycle.SigSessionCompleted},
		// Append-only for the same reason as infoleakage_taint, and a stronger
		// one: a tag's audience is an authorization input. A mutable record
		// would let a later write widen what an earlier disclosure was checked
		// against, with the audit trail agreeing.
		AppendOnly: true,
	}
}

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(TagRecord{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
