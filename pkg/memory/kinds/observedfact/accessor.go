package observedfact

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
)

// Record writes one observed fact per (subject, name) pair in obs.
//
// A thin wrapper over factcontent.Record that supplies THIS Kind's name and id
// prefix, so no caller outside this package ever passes that pair by hand. A
// genuinely mismatched pair (this Kind's name with the OTHER Kind's prefix) is
// already caught loudly — memory.Put refuses an Entry.ID that doesn't carry
// its own Kind's registered prefix, wrapping ErrInvalidEntry. What this
// wrapper actually closes is the pair that is NOT malformed: envelopefact's
// own matched (KindName, IDPrefix) is a perfectly valid pair, and passing it
// here by hand would be silently accepted, filing an envelope fact into the
// observed namespace — collapsing the trust-grade boundary the two-Kind split
// exists to keep apart (see the envelopefact package comment).
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, obs factcontent.Observation) error {
	return factcontent.Record(ctx, m, scope, KindName, IDPrefix, obs)
}

// ForSubject returns every observed fact recorded about one subject.
//
// An empty map means NOTHING HAS BEEN OBSERVED, which a precondition reads as
// undetermined. It must never be collapsed into "the facts are false".
func ForSubject(ctx context.Context, m memory.Memory, scope memory.Scope, resourceType, resourceID string) (map[string]any, error) {
	return factcontent.ForSubject(ctx, m, scope, KindName, resourceType, resourceID)
}

// SubjectsOfTypes returns every distinct object of any of `types` that an
// observed fact has been recorded about, sorted by (type, id).
//
// The wrapper exists for the same reason Record's does: the (KindName,
// IDPrefix) pair is never passed by hand, so this read can never be pointed at
// the envelope namespace by a caller that meant this one.
//
// The ids are RAW, pre-transform. See factcontent.SubjectsOfTypes, which also
// says why the list is required and may not be empty.
func SubjectsOfTypes(ctx context.Context, m memory.Memory, scope memory.Scope, types []string) ([]factcontent.Subject, error) {
	return factcontent.SubjectsOfTypes(ctx, m, scope, KindName, types)
}
