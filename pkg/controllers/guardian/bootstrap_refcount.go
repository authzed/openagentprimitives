package guardian

import (
	"sort"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
)

// Owner is one claim on a tuple by a single SpiceDBBootstrap CR.
type Owner struct {
	CR     string // "<namespace>/<name>"
	Policy string // SpiceDBBootstrapReclaim{Delete,Retain}
}

// DesiredMap is tuple → set(owner). Internal-only; expose via the
// constructor + Add to keep the iteration order stable.
type DesiredMap struct {
	tuples map[string]spicedb.Tuple
	owners map[string]map[string]Owner // tupleKey → (crName → Owner)
}

// NewDesiredMap returns an empty DesiredMap. Add tuples via Add.
func NewDesiredMap() *DesiredMap {
	return &DesiredMap{
		tuples: map[string]spicedb.Tuple{},
		owners: map[string]map[string]Owner{},
	}
}

// Add records that the named CR claims the given tuple under the given
// reclaim policy. Subsequent calls with the same tuple key extend the
// owner set without overwriting the stored tuple. Idempotent within a
// single (tuple, CR) pair: re-adding the same CR for the same tuple
// just replaces that CR's owner entry.
func (d *DesiredMap) Add(t spicedb.Tuple, o Owner) {
	key := t.Key()
	if _, ok := d.tuples[key]; !ok {
		d.tuples[key] = t
	}
	owners, ok := d.owners[key]
	if !ok {
		owners = map[string]Owner{}
		d.owners[key] = owners
	}
	owners[o.CR] = o
}

// Has reports whether any CR has claimed the tuple with the given Key().
func (d *DesiredMap) Has(key string) bool {
	_, ok := d.tuples[key]
	return ok
}

// OwnersOf returns the live owner map for the tuple keyed by `key`, or
// nil if the tuple is not in the map. The returned map is the
// internal storage — callers MUST NOT mutate it.
func (d *DesiredMap) OwnersOf(key string) map[string]Owner {
	return d.owners[key]
}

// Retain drops every tuple whose key does not satisfy keep, along with ALL
// of that tuple's owners — a tuple entry either survives whole (every
// claimant's Owner intact) or is dropped whole, so the refcount contract
// (ComputeDiff deletes a tuple only once every Delete-policy claimant has
// genuinely dropped it) stays intact: this never removes one claimant from a
// tuple that keeps other claimants.
//
// Used by agentsessiongrants_controller.go's Reconcile to prune a pass's
// newDesired down to what actually landed in SpiceDB before caching it as
// r.lastDesired — see the call site's doc comment for why a tuple whose
// TOUCH failed this pass must not survive into that belief.
func (d *DesiredMap) Retain(keep func(key string) bool) {
	for key := range d.tuples {
		if keep(key) {
			continue
		}
		delete(d.tuples, key)
		delete(d.owners, key)
	}
}

// OwnedBy returns every (tuple, owner) pair where `crName` is one of
// the owners. Used by the invalid-CR carry-forward path in
// buildNewDesired: when a CR flips Valid=True → False, we need to
// preserve whatever tuples it claimed previously, regardless of
// whether the CR's current spec still describes the same tuple
// (the invalidating edit may have changed the tuple shape — e.g.,
// flipping canonicalize=true changes the subject id).
func (d *DesiredMap) OwnedBy(crName string) []struct {
	Tuple spicedb.Tuple
	Owner Owner
} {
	var out []struct {
		Tuple spicedb.Tuple
		Owner Owner
	}
	for key, owners := range d.owners {
		if o, ok := owners[crName]; ok {
			out = append(out, struct {
				Tuple spicedb.Tuple
				Owner Owner
			}{Tuple: d.tuples[key], Owner: o})
		}
	}
	return out
}

// Diff is the per-reconcile work to perform.
type Diff struct {
	ToTouch  []spicedb.Tuple // sorted by Tuple.Key()
	ToDelete []spicedb.Tuple // sorted by Tuple.Key()
}

// ComputeDiff returns the relationships to TOUCH and DELETE.
//
//   - ToTouch is every tuple present in `new` (idempotent — SpiceDB
//     TOUCH absorbs no-ops).
//   - ToDelete is every tuple present in `last` but absent from `new`,
//     filtered to those whose `last` owners included at least one
//     reclaimPolicy=Delete claim. (Retain claims do not vote for delete.)
func ComputeDiff(last, new *DesiredMap) Diff {
	var touch []spicedb.Tuple
	for _, t := range new.tuples {
		touch = append(touch, t)
	}
	sort.Slice(touch, func(i, j int) bool { return touch[i].Key() < touch[j].Key() })

	var del []spicedb.Tuple
	for key, t := range last.tuples {
		if new.Has(key) {
			continue
		}
		// Any Delete owner from `last`?
		var deleteVote bool
		for _, o := range last.owners[key] {
			if o.Policy == spiceboxv1alpha1.SpiceDBBootstrapReclaimDelete {
				deleteVote = true
				break
			}
		}
		if deleteVote {
			del = append(del, t)
		}
	}
	sort.Slice(del, func(i, j int) bool { return del[i].Key() < del[j].Key() })

	return Diff{ToTouch: touch, ToDelete: del}
}
