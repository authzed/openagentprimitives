package relsource

import (
	"errors"
	"fmt"
	"slices"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
)

// ErrRefused is wrapped into every error CheckWrite/CheckDeleteFilter return
// when a write or delete-filter touches a relation another source claims.
// Classify a refusal with errors.Is(err, ErrRefused) — the message text
// around it (which source, which relation, which owner) is free to reword
// without breaking a caller that classifies this way, unlike matching on
// this package's "relsource: …" prefix. Distinct from
// ErrClaimTableIncomplete (complete.go): that one is a binary wiring bug
// (this process never linked the imports bundle), not a real ownership
// conflict, and must not be mistaken for one.
var ErrRefused = errors.New("relsource: refused")

// CheckWrite refuses any update whose resourceType#relation is claimed by a
// source other than src. An update touching an unclaimed relation, or one
// src itself claims, is allowed — provided the claim table has been marked
// complete; see MarkComplete.
func CheckWrite(src Source, updates []*v1.RelationshipUpdate) error {
	if err := requireComplete(); err != nil {
		return err
	}
	idx := relationIndex()
	for _, u := range updates {
		rel := u.GetRelationship()
		resourceType := rel.GetResource().GetObjectType()
		relation := rel.GetRelation()
		key := claimKey(resourceType, relation)
		if owner, claimed := idx.owner[key]; claimed && owner != src.Name {
			return fmt.Errorf("relsource: %s cannot write %s: owned by %s: %w", src.Name, key, owner, ErrRefused)
		}
	}
	return nil
}

// Owns reports whether src itself claims resourceType#relation.
//
// This is the STRICTER question CheckWrite deliberately does not answer.
// CheckWrite asks "may src write this?", and an UNCLAIMED relation is
// writable by anyone — so its silence means "nobody else owns it", never
// "src owns it". A caller that DELETES what it did not just read from its
// own upstream needs the positive form: an unclaimed relation is one
// somebody ELSE is free to write, which makes it exactly the relation a
// sync must not sweep. relsync's cross-resource reap is that caller; see
// reapAbsentCrossResourceTuples.
//
// Answered from src's own Claims rather than the process-global registry
// on purpose: Kind.Source's contract is that a source's Claims cover every
// relation it writes, so the value in hand is the authority, and a caller
// holding one gets the same answer whether or not the declaring package
// happens to be linked into this binary. For a registered source the two
// agree by construction — buildIndex panics rather than let two sources
// claim one relation.
func Owns(src Source, resourceType, relation string) bool {
	return slices.Contains(src.Claims, claimKey(resourceType, relation))
}

// CheckDeleteFilter refuses a delete filter that could match a relation
// another source owns. Refuses outright, regardless of the filter's
// contents, until the claim table has been marked complete; see
// MarkComplete.
//
// An empty ResourceType matches every relation on every type — including
// any claimed one on a type the filter never names — so it is refused
// unless src owns every claim in the whole registry. This is the case that
// would otherwise make the guard trivially bypassable: an untyped filter
// would sweep every claimed relation without naming a single one.
//
// A filter with a ResourceType and an OptionalRelation is checked like a
// single write. A filter with a ResourceType but an empty OptionalRelation
// matches every relation on that type — including any claimed one it never
// names — so it is refused unless src owns every claim registered on that
// resource type. This is the same could-match hazard as the untyped case,
// scoped to one type instead of the whole registry.
func CheckDeleteFilter(src Source, f *v1.RelationshipFilter) error {
	if err := requireComplete(); err != nil {
		return err
	}
	idx := relationIndex()
	resourceType := f.GetResourceType()
	relation := f.GetOptionalRelation()

	if resourceType == "" {
		for key, owner := range idx.owner {
			if owner != src.Name {
				return fmt.Errorf("relsource: %s cannot delete-filter with no resource type: %s owns %s and an untyped filter would sweep it: %w", src.Name, owner, key, ErrRefused)
			}
		}
		return nil
	}

	if relation != "" {
		key := claimKey(resourceType, relation)
		if owner, claimed := idx.owner[key]; claimed && owner != src.Name {
			return fmt.Errorf("relsource: %s cannot delete %s: owned by %s: %w", src.Name, key, owner, ErrRefused)
		}
		return nil
	}

	for _, claimedRelation := range idx.byType[resourceType] {
		key := claimKey(resourceType, claimedRelation)
		if owner := idx.owner[key]; owner != src.Name {
			return fmt.Errorf("relsource: %s cannot delete-filter %s with no relation: %s owns %s and an unrelation'd filter would sweep it: %w", src.Name, resourceType, owner, key, ErrRefused)
		}
	}
	return nil
}
