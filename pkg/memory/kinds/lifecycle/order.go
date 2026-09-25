package lifecycle

import (
	"context"
	"math"
	"sort"
	"time"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// SeqTerminalStop is the sentinel ordering Seq for a Stopped event emitted from
// the SIGTERM/SIGINT handler, which runs off the turn loop and so has no
// (memTurnIndex, blockIndex) context to build a real PackSeq. Max uint64 — above
// any PackSeq a runner or operator can produce — so a process-termination Stopped
// sorts AFTER every keyed event in the log, matching its causal position as the
// last thing that happened before the process died.
//
// Sorting last is load-bearing, both ways. Succeeded-then-racing-Stopped folds to
// Succeeded (the runner reached the sticky terminal first, so the later Stopped
// is a no-op), while a Stopped with no prior terminal — an admin kill of a live
// session — still folds to Failed. At Seq 0 a Stopped would sort BEFORE every
// keyed event, fold to Failed first and stick, wrongly losing a real Succeeded.
const SeqTerminalStop = uint64(math.MaxUint64)

// TerminalStopKey is the ordering key for a process-termination Stopped event
// emitted by the AgentSession instance sessionUID.
//
// Region is RegionOperatorPost for two reasons: it makes the key stamped (so the
// Seq is persisted rather than dropped as a legacy zero key), and a non-runner
// region is skipped by the operator's maxRunnerTurn scan, which must never
// observe the sentinel Seq as a real turn index.
//
// SessionUID matters here even though the sentinel Seq is unique-max and so
// feeds no same-Seq tiebreak: a Stopped with no prior terminal folds to Failed
// on its own, and a scope outlives the CR that wrote into it (see
// ForIncarnation). An unattributed Stopped would hand that terminal to whatever
// AgentSession next holds the name.
func TerminalStopKey(sessionUID string) OrderKey {
	return OrderKey{Seq: SeqTerminalStop, Region: string(lc.RegionOperatorPost), SessionUID: sessionUID}
}

// ForIncarnation returns the subset of evs belonging to the AgentSession
// instance sessionUID — the events that instance's fold may consume.
//
// A scope is keyed by namespace/name, and the append-only entries in it are
// PERMANENT: Local.DeleteScope cannot remove them, so they outlive both the
// AgentSession CR and its deletion. A trigger that derives one session name per
// external subject (a pull request, a chat thread) therefore re-creates a
// deleted session under a name whose log already holds a previous instance's
// events — including, when that instance failed, a sticky terminal one. Folding
// the whole log would make the fresh CR terminal before it ran a turn, and no
// redelivery could ever recover it.
//
// An event carrying NO SessionUID is kept for every instance. That is the shape
// of every entry written before the ordering key existed, so excluding them
// would blank a live session's own history the moment the operator upgraded. An
// empty sessionUID (a caller that cannot identify its instance — an unnamed test
// fixture) likewise keeps everything: filtering on an unknown identity would
// drop the whole log.
//
// The result preserves the input's fold order; neither the input nor its
// elements are mutated.
func ForIncarnation(evs []OrderedEvent, sessionUID string) []OrderedEvent {
	if sessionUID == "" {
		return evs
	}
	out := make([]OrderedEvent, 0, len(evs))
	for i := range evs {
		if uid := evs[i].Key.SessionUID; uid == "" || uid == sessionUID {
			out = append(out, evs[i])
		}
	}
	return out
}

// OrderKey is the durable cross-publisher fold-ordering key carried inside each
// lifecycle event envelope. It rides in the entry's Content, which the
// append-only provenance layer already folds into the signed EntryDigest — so
// carrying it needs NO change to the entry schema, the digest, or the
// per-(scope, publisher) hash chain, and `oap audit verify` keeps working.
//
// Two processes append to a session's lifecycle log — the operator
// (system:operator, pre/post regions) and the runner (session:<ns/name>, live
// region) — stamping createdAt from independent clocks, so folding by wall clock
// lets a lagging operator event sort after a causally-later runner event. This
// key replaces wall clock with a skew-immune logical order.
type OrderKey struct {
	// Seq is PackSeq(memTurnIndex, blockIndex): the same durable per-session
	// logical order the channel status stream uses. memTurnIndex is monotonic
	// across resume (rebuilt by the runner's replay), so events of a later runner
	// incarnation sort after an earlier one regardless of clock skew between the
	// two processes. Zero on an unstamped (legacy) entry.
	Seq uint64 `json:"seq,omitempty"`
	// Region is the emitting authority region: RegionOperatorPre, RegionRunner,
	// or RegionOperatorPost. At equal Seq it orders a single turn's events —
	// pre-handoff provisioning before the runner's live events before the
	// post-handoff backstop — so a stale operator RunnerClaimed (operator_pre)
	// can never sort after the runner's terminal event of the same turn and
	// re-promote a rested session. Empty on an unstamped (legacy) entry.
	Region string `json:"region,omitempty"`
	// SessionUID records the AgentSession instance for fork/restart lineage.
	// Within one scope it is constant across runner respawns (the AgentSession
	// UID is stable); it is recorded for a deterministic final tiebreak and for
	// future cross-lineage ordering, not as a primary sort key.
	SessionUID string `json:"uid,omitempty"`
}

// stamped reports whether this key was written by a publisher that stamps the
// ordering key. Legacy entries (written before the key existed) carry no Region
// and fall back to createdAt ordering.
func (k OrderKey) stamped() bool { return k.Region != "" }

// regionRank maps a region to its within-turn sort position. Unstamped/unknown
// sorts first at equal Seq, so a fully-legacy log (all Seq 0, all rank 0) folds
// purely by createdAt.
func regionRank(region string) int {
	switch region {
	case string(lc.RegionOperatorPre):
		return 1
	case string(lc.RegionRunner):
		return 2
	case string(lc.RegionOperatorPost):
		return 3
	default:
		return 0
	}
}

// OrderedEvent pairs a decoded transition event with its ordering key, in fold
// order. ReadOrdered returns these; the operator uses the keys to observe the
// highest runner turn index before stamping its own events. EntryID and CreatedAt
// are what make an INCREMENTAL read possible (see IncrementalReader); neither
// participates in the fold, which consumes only Event.
type OrderedEvent struct {
	// EntryID is the memory Entry.ID this event was decoded from. Unique
	// within the scope, and stable across re-reads, so it is the dedup key
	// for a lookback-window read that returns entries already merged.
	EntryID string
	// Event is the decoded transition itself — the only field Fold consumes.
	Event lc.Event
	// Key is the skew-immune logical order; zero on an unstamped legacy entry.
	Key OrderKey
	// CreatedAt is the entry's wall-clock stamp, written by whichever
	// publisher appended it. It is the tiebreak in the total order AND the
	// only value memory.Query can filter a tail on (Query.Since), which is
	// why an incremental read needs a lookback window rather than a bare
	// high-water mark — see IncrementalLookback.
	CreatedAt time.Time
}

// decoded is the internal sort unit: an event plus the two things the total
// order compares — its logical key and its wall-clock stamp (tiebreak + legacy
// fallback) — plus the entry ID it came from, which the total order ignores and
// MergeOrdered dedupes on.
type decoded struct {
	id    string
	event lc.Event
	key   OrderKey
	at    time.Time
}

// less is the total order over decoded events. It is a plain lexicographic
// tuple comparison — (Seq, regionRank, createdAt, SessionUID) — which is
// transitive by construction, so it is a valid strict weak ordering for sort
// even across a mix of stamped and legacy entries. Seq dominates, so the
// wall-clock skew that misorders a two-publisher log cannot reorder events that
// carry distinct logical sequence numbers.
func less(a, b decoded) bool {
	if a.key.Seq != b.key.Seq {
		return a.key.Seq < b.key.Seq
	}
	if ra, rb := regionRank(a.key.Region), regionRank(b.key.Region); ra != rb {
		return ra < rb
	}
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	return a.key.SessionUID < b.key.SessionUID
}

// ReadOrdered reads the scope's typed transition events in skew-immune fold
// order (see OrderKey). Recorded signal entries (no EventTag) are skipped, as is
// an event-tagged entry that will not decode — logged, not fatal, because an
// append-only entry cannot be deleted and a hard error would be permanent.
//
// This is the WHOLE log, every call: O(E) bytes over HTTP and O(E log E) to
// decode and sort. Fine for a caller that reads once per session (the operator's
// reconcile); a caller that reads once per emitted event wants IncrementalReader
// instead, or it pays O(E²) across the session.
func ReadOrdered(ctx context.Context, m memory.Memory, scope memory.Scope) ([]OrderedEvent, error) {
	return ReadOrderedSince(ctx, m, scope, time.Time{})
}

// ReadOrderedSince is ReadOrdered restricted to the tail: only entries whose
// createdAt is at or after since (a zero since reads everything). The filter is
// memory.Query.Since, an INCLUSIVE created_at >= bound in every backend.
//
// A tail read is NOT a safe incremental read on its own. createdAt comes from
// the appending publisher's own clock, and two publishers append here (the runner
// as session:<ns/name>, the operator as system:operator) — the exact skew
// OrderKey exists to defeat — so a bound derived from the newest entry one has
// seen can sit AFTER an entry the other is about to write with an earlier stamp.
// Callers must subtract a lookback window from their watermark and dedupe the
// overlap by EntryID, which is what IncrementalReader does. Use this directly
// only when you genuinely want "the tail since T" and can tolerate that.
//
// The result uses the same total order as ReadOrdered, so MergeOrdered accepts it.
func ReadOrderedSince(ctx context.Context, m memory.Memory, scope memory.Scope, since time.Time) ([]OrderedEvent, error) {
	q := memory.Query{
		Scope:   scope,
		Kinds:   []string{Kind{}.Name()},
		Tags:    []string{EventTag},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	}
	if !since.IsZero() {
		s := since
		q.Since = &s
	}
	res, err := m.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	ds := make([]decoded, 0, len(res.Entries))
	for i := range res.Entries {
		e := res.Entries[i]
		if !hasTag(e.Tags, EventTag) {
			// Defensive: a backend that does not honor the Tags filter must not
			// hand a recorded-signal payload to the envelope decoder.
			continue
		}
		ev, key, err := decodeEnvelope(e.Content)
		if err != nil {
			// Skipped, not fatal. This Kind is BOTH append-only and session-
			// written, so a runner may append an EventTag-tagged entry whose
			// content is not an envelope — and nothing can then remove it:
			// per-entry Delete is refused unconditionally for an append-only
			// Kind, and DeleteScope skips it. A hard error here would make every
			// read of the scope fail forever, and the AgentSession reconciler
			// folds the log on every pass, so one malformed record would wedge
			// the session permanently with no operator remedy.
			//
			// Logged with the entry ID and its publisher: that names the writer
			// to go fix, and a skipped event is a GAP in a tamper-evident
			// timeline — the lesser harm, but not a free one.
			undecodable.Skipped("lifecycle.ReadOrdered", scope, e, err)
			continue
		}
		ds = append(ds, decoded{id: e.ID, event: ev, key: key, at: e.CreatedAt})
	}
	// Query already ordered by createdAt; a stable sort by the logical key keeps
	// that as the tiebreak for entries the key leaves equal.
	sort.SliceStable(ds, func(i, j int) bool { return less(ds[i], ds[j]) })
	out := make([]OrderedEvent, len(ds))
	for i := range ds {
		out[i] = OrderedEvent{EntryID: ds[i].id, Event: ds[i].event, Key: ds[i].key, CreatedAt: ds[i].at}
	}
	return out, nil
}
