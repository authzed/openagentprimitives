// Package trifecta derives the three legs whose combination makes a delegation
// dangerous, and judges them.
//
// THE TRIFECTA IS A PROPERTY OF THE CLOSURE, NOT OF ANY NODE IN IT. A parent
// that reads untrusted content is fine. A child that can write is fine. The
// delegation that joins them is the thing to refuse — which is why this is
// evaluated over a HANDOFF at binding time rather than over a session, and why
// no single node's configuration can be inspected to find it.
package trifecta

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
)

// Legs are the three properties, derived and unjudged.
type Legs struct {
	// Untrusted (A): some bound input carries content that may be hostile.
	//
	// ARGUMENT-level, not context-level: the question is whether untrusted
	// content can determine an authority-bearing argument, which is what a
	// bound input does. Content merely present in a transcript is a weaker and
	// far noisier signal.
	Untrusted bool

	// Sensitive (B): some bound input's reader set is NARROWER than the
	// child's own reach, so binding it discloses to someone who could not
	// already see it.
	//
	// Not "is this data secret" — narrower-than-where-it-is-going. A tag
	// everyone on the child can already read is not sensitive access in this
	// sense, however confidential it feels.
	Sensitive bool

	// Consequential (C): the child can ACT — readwrite or external.
	//
	// Readonly is deliberately excluded. Being able to read is leg B's
	// territory; treating any surface membership as consequential would fire
	// the trifecta on a child that can only look at things, which is a
	// combination the design permits.
	Consequential bool
}

// All reports whether every leg holds. A READING of the legs, not a decision
// about them — the judgement is Evaluate's, and the enforcement decision the
// hook's.
func (l Legs) All() bool { return l.Untrusted && l.Sensitive && l.Consequential }

// Deps are the three lookups a derivation needs.
type Deps struct {
	// TagCarriesUntrusted answers `pt_tag#carries_untrusted`.
	TagCarriesUntrusted func(ctx context.Context, tagID string) (bool, error)
	// TagReaders answers `pt_tag#reader` — the tag's fully-resolved audience,
	// with the intersection over its derivation tree already applied.
	TagReaders func(ctx context.Context, tagID string) ([]string, error)
	// ChildAudience answers `agentsession#read_transcript` for the child: who
	// would be able to see the datum once it is bound there.
	ChildAudience func(ctx context.Context, child authz.SessionRef) ([]string, error)
}

// Handoff is the delegation being judged.
type Handoff struct {
	// Child is the session about to be spawned.
	Child authz.SessionRef
	// BoundInputs are the pt-tag ids the parent is binding into its data slots.
	BoundInputs []string
	// ChildSurface is the child's enumerated permission surface.
	//
	// Absence from this surface means CANNOT ACT, and that reading is only
	// sound because tool.FatalFor now refuses to start a runner whose envelope
	// has an unhandleable tool while the surface is enforced. Before that, such
	// a tool was excluded from the surface yet still dispatchable, and leg C
	// would have read clean for a child that could write.
	ChildSurface []permsurface.Descriptor
}

// Derive computes the three legs. It applies no policy and returns no verdict:
// a caller may want to log legs without being handed a refusal, and the
// operator-side containment judgement reaches a different conclusion from the
// same facts than the dispatch-time gate does.
//
// EVERY unresolvable lookup is an error. A leg that silently read false would
// be the trifecta failing to fire — no denial, no hold, no log, nothing
// anywhere to notice — which is the worst outcome this package can produce.
// The caller decides what an unknown means; it must not inherit "safe" here.
func Derive(ctx context.Context, d Deps, h Handoff) (Legs, error) {
	var legs Legs

	// Leg C first: it needs no lookups at all, so a handoff that cannot
	// possibly be consequential is settled without touching SpiceDB.
	for _, desc := range h.ChildSurface {
		if desc.StateImpact == authz.Readwrite || desc.StateImpact == authz.External {
			legs.Consequential = true
			break
		}
	}

	if len(h.BoundInputs) == 0 {
		// Nothing bound: no input can be untrusted and none can be narrower
		// than the child. Legs A and B are false as a FACT about this handoff,
		// not as a default.
		return legs, nil
	}

	if d.TagCarriesUntrusted == nil || d.TagReaders == nil || d.ChildAudience == nil {
		return Legs{}, fmt.Errorf("trifecta: derivation is not fully wired")
	}

	audience, err := d.ChildAudience(ctx, h.Child)
	if err != nil {
		return Legs{}, fmt.Errorf("trifecta: audience of child %s/%s: %w", h.Child.Namespace, h.Child.Name, err)
	}

	for _, tagID := range h.BoundInputs {
		untrusted, err := d.TagCarriesUntrusted(ctx, tagID)
		if err != nil {
			return Legs{}, fmt.Errorf("trifecta: integrity of tag %q: %w", tagID, err)
		}
		if untrusted {
			legs.Untrusted = true
		}

		readers, err := d.TagReaders(ctx, tagID)
		if err != nil {
			return Legs{}, fmt.Errorf("trifecta: readers of tag %q: %w", tagID, err)
		}
		// Narrower than the child means someone in the child's audience is not
		// among the tag's readers. The same predicate both egress checks, slot
		// grading and handoff grading use — one safety rule, written once.
		if len(provenance.Unauthorized(audience, readers)) > 0 {
			legs.Sensitive = true
		}
	}
	return legs, nil
}
