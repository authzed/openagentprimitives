package memory

import (
	"hash/maphash"
	"sync"
)

// appendOnlyLockStripes is the fixed number of mutexes the append-only write
// lock spreads keys across.
//
// Fixed stripes rather than a map of per-key mutexes: entry IDs are unbounded
// and almost always unique, so a keyed map would grow for the life of the
// process and need a reaper (and a reaper needs a refcount, which is a second
// synchronization problem). 256 mutexes cost a few kilobytes once and need no
// cleanup at all.
//
// The price is false sharing — two unrelated entries whose keys hash to one
// stripe serialize against each other — and how affordable that is depends on
// how long the lock is actually held, so be precise about it. The critical
// section is NOT just two backend calls. It spans the pre-check Get, the
// configured ProvenanceVerifier's Ed25519 signature verification, the
// Authorizer's AuthorizePut — which issues a SpiceDB WriteRelationships
// whenever a SpiceDB client is wired (pkg/memory/spicedbauthorizer) — and the
// backend Put. So a colliding pair can wait on a signature check plus a SpiceDB
// round trip plus a Postgres write, not on a map lookup. 256 stripes keeps the
// odds of that at 1 in 256 for any given pair, and append-only writes are
// audit/transcript records rather than a latency-critical path. Widen the
// stripe count before widening the critical section.
const appendOnlyLockStripes = 256

// appendOnlyKey identifies the entry an append-only write is racing for. It is
// the backend's own primary key for an entry, field for field: inmem keys on
// {Scope.Kind, Scope.ID} plus {Kind, ID}, and the SQL backends carry the same
// four columns. Scope.Kind is part of it even though "session" is the only
// member in use today, because a key narrower than the backend's would let two
// genuinely distinct entries contend, and one wider would let two writers of
// one real entry through — and only the second of those is a correctness bug.
type appendOnlyKey struct {
	scopeKind string
	scopeID   string
	kind      string
	id        string
}

// stripedKeyedMutex is a keyed mutex over a fixed array of stripes.
type stripedKeyedMutex struct {
	seed   maphash.Seed
	stripe [appendOnlyLockStripes]sync.Mutex
}

func newStripedKeyedMutex() *stripedKeyedMutex {
	// A per-process random seed. Only stripe SELECTION depends on the hash, so a
	// collision costs contention and never correctness — but part of the key is a
	// caller-chosen entry ID, and a randomized seed means no caller can pick IDs
	// that funnel every write onto one stripe. maphash gives that for free.
	return &stripedKeyedMutex{seed: maphash.MakeSeed()}
}

// Lock acquires the stripe guarding k and returns the release func.
//
// The release is idempotent, so a caller may BOTH defer it — as the safety net
// covering every early return between acquisition and the end of the critical
// section — and call it explicitly at the point the critical section actually
// ends. Whichever runs first releases; the other is a no-op.
func (s *stripedKeyedMutex) Lock(k appendOnlyKey) (unlock func()) {
	// maphash.Comparable over the struct, NOT a hash of the four fields
	// concatenated: concatenation would let ("a\x00b", "c") and ("a", "b\x00c")
	// collide into one key, which is a correctness question rather than a
	// contention one if it ever became a key equality test.
	mu := &s.stripe[maphash.Comparable(s.seed, k)%appendOnlyLockStripes]
	mu.Lock()
	var once sync.Once
	return func() { once.Do(mu.Unlock) }
}

// appendOnlyWriteLocks serializes the check-then-write span of an append-only
// Put in Local.Put: the pre-check Get that decides the entry is new, and the
// backend Put that stores it. Without it two writers of DIFFERENT content for
// one (scope, kind, id) both miss the Get and the second silently overwrites
// the first, defeating the write-once property every append-only Kind — and
// `oap audit verify` — depends on.
//
// WHAT THIS LOCK DOES NOT PROTECT, and why that is currently enough.
//
// It serializes writers WITHIN ONE PROCESS. It is sufficient only because the
// operator is the single writer of the durable memory store: the operator's
// Deployment is replicas: 1 with strategy: Recreate (config/manager/
// deployment.yaml), and every other component — runner, channelsd, authzd,
// webd — writes through the memory HTTP API into that one process's Local
// facade, so all append-only writes really do funnel through this one lock.
//
// At TWO replicas it silently stops protecting anything: two operator
// processes hold two independent stripe arrays, both miss the same pre-check
// Get, and the race is exactly back — with no error, no log line, and nothing
// that fails until someone runs `oap audit verify`. The correct fix at that
// point is not a bigger lock but an atomic compare-and-set in the backend
// (PutIfAbsent), which is enumerated with the rest of what HA requires in the
// TODO block at the top of internal/cmd/operator/main.go.
//
// TestOperatorDeploymentStaysSingleReplica (pkg/platform/manifests) is the
// tripwire: it fails the day the operator's replica count is raised, and says
// to come read this.
var appendOnlyWriteLocks = newStripedKeyedMutex()
