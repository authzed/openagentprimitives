package pttag

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/auditaccessor"
)

// List returns every pt_tag minted in scope (oldest-first).
//
// Read-only, and deliberately so: there is no matching Record here, unlike the
// audit kinds beside it. A tag is written by the MINTER and by nothing else —
// `direct_reader` grants disclosure, so an accessor that let any holder of a
// memory handle append a tag would hand callers the one capability the mint
// route exists to withhold. Whoever needs to write one needs the operator's
// minter, not a shorter path to the same store.
func List(ctx context.Context, m memory.Memory, scope memory.Scope) ([]TagRecord, error) {
	return auditaccessor.List[TagRecord](ctx, m, scope, Kind{}, "pttag.List")
}

// SourcesOf returns the "type:id" objects one tag was minted from.
//
// The route to a human who can rule on disclosing a datum. `pt_tag` has no
// owner relation — session, granted_to, derived_from, direct_reader,
// untrusted_origin, and nothing else — so an owner is reachable only through
// the objects the tag came from.
//
// An unknown tag and a tag with no recorded sources both return nil with no
// error: neither is a failure, and both mean the same thing to a caller, which
// is that no owner set can be derived and it must fall back to whatever
// deciders it has otherwise.
func SourcesOf(ctx context.Context, m memory.Memory, scope memory.Scope, tagID string) ([]string, error) {
	recs, err := List(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.ID == tagID {
			return r.Sources, nil
		}
	}
	return nil, nil
}
