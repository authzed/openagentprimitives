package relsource

// SentinelRelation is the relationship-set hash relation the sync engine
// writes alongside real scope content (`<definition>#relhash@string:<sha256>`).
//
// Declared HERE, rather than in the sync engine that writes it, because this
// is the package every consumer can reach: pkg/platform/relsync imports
// pkg/authz/spicedb, so the fact cannot live there without cycling for any
// spicedb-side reader. relsync keeps its own internal constant and a test
// asserting the two agree, so the duplication cannot drift silently.
const SentinelRelation = "relhash"

// IsSentinelRelation reports whether relation is that sentinel. A reader
// walking claimed relations for a USER subject can skip it: the sentinel's
// subject is a string digest by construction, never a user, so probing it can
// only ever return nothing.
func IsSentinelRelation(relation string) bool { return relation == SentinelRelation }
