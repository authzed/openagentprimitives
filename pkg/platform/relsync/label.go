package relsync

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/authz/resourcedisplay"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// ScopeLabelTuple builds the display-name tuple for one scope —
// `<ResourceType>:<ID>#label@string:<base64url of name>` — or reports that the
// name upstream gave is not renderable.
//
// # A label is ordinary scope content, not a second sentinel
//
// It looks like the #relhash sentinel (same `string` subject type, same
// "SpiceDB has no string-valued relation" workaround) and is deliberately NOT
// written like one. The sentinel is engine bookkeeping: Pass writes it
// separately, excludes it from the per-scope read, and CASes on it. A label is
// returned by a kind's FetchScope inside ScopeContent.Tuples like any
// membership edge, and every consequence of that is one this feature wanted:
//
//   - It is part of the scope's CONTENT HASH. This is what makes the backfill
//     happen at all. Pass skips a scope whose stored hash still matches, so a
//     directory synced before labels existed would otherwise never be written
//     again and the console would show raw ids forever. Adding a tuple changes
//     the set, changes the hash, and the next pass writes the scope — once,
//     after which the hash matches again and the skip resumes. No flag, no
//     forced rewrite, no migration.
//   - It is DIFFED and PRUNED. A renamed channel's old label tuple is a
//     removal in the same write that adds the new one, because the name rides
//     as a subject id and a changed name is a different tuple — the exact
//     problem diffScope solves for the sentinel by hand, solved here by the
//     ordinary path.
//   - It is on the scope's OWN resource object, so it is neither
//     cross-resource content (recordExtraResources skips it) nor exempt from
//     the whole-object sweep a reaped scope gets.
//
// # What it is not
//
// Display data. The subject type declares no permission and the relation
// appears in no permission expression, so nothing can be granted or denied by
// a name — see relsource.LabelRelation for the full argument, including the
// registration-time guard that keeps a label out of identity probes.
//
// ok is false when name has nothing renderable left after
// resourcedisplay.SanitizeLabelText — an empty name, or one made entirely of
// control and format characters. The caller MUST NOT append the zero Tuple in
// that case: the scope simply keeps rendering by its raw id, which is what it
// did before labels existed.
//
// A dropped label is LOGGED rather than returned as an error, and the split is
// deliberate. Failing the fetch would let one oddly-named channel stop that
// channel's MEMBERSHIP from syncing — trading a correctness problem for a
// cosmetic one — but dropping it silently is the no-silent-errors failure mode
// exactly: an unnamed row looks identical whether the directory stores no name
// or this function refused the one it had. The log line is what an operator
// can grep when a single row will not take a name.
func ScopeLabelTuple(ctx context.Context, scope Scope, name string) (spicedb.Tuple, bool) {
	encoded, ok := resourcedisplay.EncodeB64Text(name)
	if !ok {
		if name != "" {
			// Only worth a line when there WAS something upstream: a scope with
			// a genuinely empty name is not a failure, it is a directory that
			// has no name for it.
			logr.FromContextOrDiscard(ctx).Info(
				"relsync: upstream name for a scope is not renderable; the scope will show its raw id",
				"definition", scope.ResourceType, "scope", string(scope.ID), "nameBytes", len(name))
		}
		return spicedb.Tuple{}, false
	}
	return spicedb.Tuple{
		ResourceType: scope.ResourceType,
		ResourceID:   string(scope.ID),
		Relation:     relsource.LabelRelation,
		SubjectType:  relsource.LabelSubjectType,
		SubjectID:    encoded,
	}, true
}
