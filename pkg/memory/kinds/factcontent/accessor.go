package factcontent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
)

// Subject is one object an observation was about.
type Subject struct {
	ResourceType string
	ResourceID   string
}

// Observation is one payload's worth of co-derived subjects and facts.
//
// The two travel together in one struct for the reason the whole design turns
// on: a fact and the object it describes come out of the same payload, so
// there must be no API that lets a caller supply them separately.
type Observation struct {
	Subjects []Subject
	Facts    map[string]any
	Source   Source
	// ObservationID groups the entries this produces. Empty means Record mints
	// one; a caller with its own correlation id (a tool_use id, a delivery id)
	// should pass it so an auditor can join across records.
	ObservationID string
}

// Record writes one entry per (subject, fact) pair.
//
// The fan-out is the point: every subject the payload named carries every fact
// the payload asserted, so a later reader that resolves one subject finds the
// facts without re-deriving any identity.
//
// All-or-nothing is NOT attempted, and that is safe here in a way it would not
// be for a grant: every entry is independently keyed and append-only, so a
// partial write leaves a strict subset of true facts. The failure mode is a
// precondition reading `undetermined` and the agent re-observing — never a
// fact that is wrong. The error still propagates so the caller can surface it.
func Record(ctx context.Context, m memory.Memory, scope memory.Scope, kindName, idPrefix string, obs Observation) error {
	if m == nil {
		// A nil Memory is a caller wiring bug, not "nothing to record" — the
		// two must not share a return value. Returning nil here would convert
		// "recorded" into "undetermined forever", and a later gate reads
		// undetermined as deny: the caller would believe a fact was written
		// and it silently never was. This check catches a genuinely nil
		// interface only — a typed-nil concrete pointer (e.g. a nil *Local)
		// assigned into the Memory interface field compares non-nil here and
		// slips past, per this repo's own rule on that construction; the fix
		// for that class lives at the assignment site, not here.
		return fmt.Errorf("factcontent.Record: m is nil")
	}
	if len(obs.Subjects) == 0 || len(obs.Facts) == 0 {
		// Genuinely nothing to record — not a failure. A caller that observed
		// no subjects or asserted no facts asked for a no-op, not an error.
		return nil
	}
	obsID := obs.ObservationID
	if obsID == "" {
		var err error
		obsID, err = newObservationID()
		if err != nil {
			return fmt.Errorf("factcontent.Record: mint observation id: %w", err)
		}
	}
	now := time.Now().UTC()
	for _, subj := range obs.Subjects {
		if subj.ResourceType == "" || subj.ResourceID == "" {
			return fmt.Errorf("factcontent.Record: subject with empty type or id (%q:%q) — a fact with no subject is exactly what the design forbids",
				subj.ResourceType, subj.ResourceID)
		}
		for name, val := range obs.Facts {
			c := Content{
				ResourceType:  subj.ResourceType,
				ResourceID:    subj.ResourceID,
				Name:          name,
				Value:         val,
				ObservationID: obsID,
				Source:        obs.Source,
			}
			raw, err := json.Marshal(c)
			if err != nil {
				return fmt.Errorf("factcontent.Record: marshal %s/%s %s: %w", subj.ResourceType, subj.ResourceID, name, err)
			}
			id := idPrefix + EntryID(subj.ResourceType, subj.ResourceID, name)

			_, err = m.Put(ctx, memory.Entry{
				Scope:     scope,
				Kind:      kindName,
				ID:        id,
				CreatedAt: now,
				Content:   raw,
			})
			if err == nil {
				continue
			}

			// RULING T1-A: compare the VALUE ourselves before deciding a
			// conflict is real.
			//
			// The facade's append-only door compares the WHOLE canonical entry
			// (facade.go entriesEquivalent), and Content carries ObservationID
			// and Source, which are per-observation by design (see their doc
			// comments in content.go). So a second call that re-derives the
			// SAME value from a new tool_use id or delivery lands here as
			// ErrAppendOnlyConflict even though nothing contradictory
			// happened — turning an ordinary repeat observation into an error
			// the agent cannot act on.
			//
			// What write-once must mean here is that the VALUE is fixed, not
			// that the provenance is. So: read back what is actually stored;
			// an equal Value is a no-op and the FIRST observation's
			// provenance stands untouched; a DIFFERENT value is the
			// contradiction attempt, and that must still fail loudly.
			//
			// Same shape as triggerdelivery.Record
			// (kinds/triggerdelivery/accessor.go), which likewise reads the
			// stored entry back and compares before treating a conflict as a
			// no-op — "could not verify" is never collapsed into "genuinely
			// different".
			if !errors.Is(err, memory.ErrAppendOnlyConflict) {
				return fmt.Errorf("factcontent.Record: put %s/%s %s: %w", subj.ResourceType, subj.ResourceID, name, err)
			}
			existing, found, gerr := factValue(ctx, m, scope, kindName, id)
			if gerr != nil {
				return fmt.Errorf("factcontent.Record: fact %s:%s %s already recorded and could not be read back "+
					"to compare (read-back error: %v): %w", subj.ResourceType, subj.ResourceID, name, gerr, err)
			}
			if !found {
				return fmt.Errorf("factcontent.Record: fact %s:%s %s reported an append-only conflict but the "+
					"read-back found no existing entry, so it could not be compared: %w",
					subj.ResourceType, subj.ResourceID, name, err)
			}
			same, cerr := sameFactValue(existing, val)
			if cerr != nil {
				return fmt.Errorf("factcontent.Record: fact %s:%s %s already recorded and this value could NOT "+
					"be compared against it (compare error: %v), so whether the two differ is UNKNOWN: %w",
					subj.ResourceType, subj.ResourceID, name, cerr, err)
			}
			if same {
				continue // same value, already established by an earlier observation — nothing to do
			}
			return fmt.Errorf("%w: fact %s:%s %s already recorded with a different value",
				memory.ErrAppendOnlyConflict, subj.ResourceType, subj.ResourceID, name)
		}
	}
	return nil
}

// sameFactValue reports whether a stored fact value and a freshly derived one
// are the same fact seen twice.
//
// The two sides are NOT comparable as Go values, and comparing them directly
// is a false tamper accusation waiting to happen. `stored` has been through
// JSON: every number is a float64, every list a []any, every object a
// map[string]any. `fresh` came straight out of CEL, where `size()`, `int()`,
// arithmetic and integer literals yield int64 and a list literal or
// comprehension yields []ref.Val. reflect.DeepEqual answers false for
// float64(2) against int64(2), so an identical re-observation of any
// non-boolean fact would be reported as a contradiction — inside the
// subsystem whose entire job is to tell a real contradiction from a repeat.
//
// So compare canonically, the same way triggerdelivery.Record compares its
// stored body: re-marshal both sides and compare the bytes. Marshaling is the
// right canonical form here rather than merely a convenient one — the fresh
// value's JSON encoding is literally what Put wrote and what a later reader
// will decode, so two values with the same encoding ARE the same fact, and
// Go's encoder sorts map keys, so object field order cannot make them differ.
//
// A marshal failure is returned, never swallowed: "could not compare" must not
// collapse into either verdict.
func sameFactValue(stored, fresh any) (bool, error) {
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		return false, fmt.Errorf("marshal stored value: %w", err)
	}
	freshJSON, err := json.Marshal(fresh)
	if err != nil {
		return false, fmt.Errorf("marshal new value: %w", err)
	}
	return bytes.Equal(storedJSON, freshJSON), nil
}

// factValue reads back the Value stored at id, or (nil, false, nil) when
// nothing is stored there yet. Used only to decide whether an append-only
// conflict is a genuine contradiction or an equal-value re-observation.
func factValue(ctx context.Context, m memory.Memory, scope memory.Scope, kindName, id string) (any, bool, error) {
	res, err := m.Query(ctx, memory.Query{Scope: scope, Kinds: []string{kindName}, IDs: []string{id}})
	if err != nil {
		return nil, false, err
	}
	if len(res.Entries) == 0 {
		return nil, false, nil
	}
	var c Content
	if err := json.Unmarshal(res.Entries[0].Content, &c); err != nil {
		return nil, false, fmt.Errorf("undecodable entry %s: %w", id, err)
	}
	return c.Value, true, nil
}

// newObservationID mints a bare correlation handle for a caller that supplied
// none. Not memory.NewID: that mints a Kind-prefixed ENTRY id, and an
// ObservationID is never used as an id itself — it only rides inside several
// entries' Content so an auditor can join them back to one payload. Not a
// security value, so 8 random bytes is enough entropy to avoid an accidental
// collision within one observation's lifetime.
func newObservationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "obs-" + hex.EncodeToString(b[:]), nil
}

// ForSubject returns every fact of kindName recorded about one subject, keyed
// by fact name.
//
// resourceID is the RAW provider identifier, pre-transform — `demo-org/
// demo-repo#6`, not the `spicedb_escape`d object id a slot's ValueTransforms
// would produce. A caller holding a slot's transform chain must look up with
// the value from BEFORE it runs and transform only for the SpiceDB call; see
// the package doc comment, which spells out why a post-transform lookup misses
// permanently and silently.
//
// An empty map means NOTHING HAS BEEN OBSERVED, which a precondition reads as
// undetermined. It must never be collapsed into "the facts are false" — that
// collapse is the bypass the whole mechanism exists to prevent.
//
// # The test is KEY PRESENCE, never value non-nilness — read with comma-ok
//
// A present key whose value is nil is a fact the tool ACTUALLY REPORTED, whose
// value happens to be null (a CEL expression may legitimately yield null, and
// pkg/authz/relwrites.EvalAnyWithItem converts it to a Go nil so it stores and
// decodes as JSON null rather than as some stand-in number). An ABSENT key is
// undetermined. The two must never be conflated, because they oblige a gate to
// do opposite things: undetermined must never bind, while an observed null is
// evidence like any other value and the gate may act on it.
//
//	v, ok := facts[name]   // correct: ok distinguishes the two
//	v := facts[name]       // WRONG: nil now means both "null" and "unobserved"
//
// The bare form is written down as wrong here because it is the shape that
// compiles, reads naturally, and silently turns an observed null into
// undetermined — failing closed on real evidence, which looks like nothing at
// all rather than like a bug.
func ForSubject(ctx context.Context, m memory.Memory, scope memory.Scope, kindName, resourceType, resourceID string) (map[string]any, error) {
	// FieldEquals is an optimization for a backend that indexes content, not a
	// filter this function may rely on for correctness. A backend that does
	// not declare Capabilities.ContentSchemas drops the predicate into
	// DroppedPredicates and answers with the WHOLE Kind in scope — every
	// subject, not just this one. Trusting that here would let a fact
	// observed about a DIFFERENT resource (PR #5) answer a question asked
	// about this one (PR #6): exactly the laundering this whole mechanism
	// exists to prevent, and it would be invisible in this package's own
	// tests, since inmem does honor the predicate. So the Go-side check below
	// is load-bearing, not redundant — same posture as
	// infoleakagedecision.hasDecision (kinds/infoleakagedecision/accessor.go),
	// which states the identical reasoning for the identical shape of read.
	res, err := m.Query(ctx, memory.Query{
		Scope: scope,
		Kinds: []string{kindName},
		FieldEquals: []memory.FieldFilter{
			{Path: "resourceType", Value: resourceType},
			{Path: "resourceID", Value: resourceID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("factcontent.ForSubject: query %s %s:%s: %w", kindName, resourceType, resourceID, err)
	}
	out := make(map[string]any, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// An append-only fact row that will not decode is a corrupt or
			// tampered record. Skipping it here is correct — the caller must
			// see "not observed" for THIS fact name, which fails closed the
			// same way an unobserved subject does — but it must be LOUD,
			// because a fact vanishing from a gate's input is precisely what
			// the tamper-evident subsystem exists to surface.
			undecodable.Skipped("factcontent.ForSubject", scope, e, err)
			continue
		}
		if c.ResourceType != resourceType || c.ResourceID != resourceID {
			// The authoritative filter. A backend that dropped FieldEquals
			// above would otherwise hand this entry to the caller unfiltered;
			// silently discarding it here (not logging) is correct because it
			// is not corrupt or missing evidence — it is a real, decodable
			// fact about a DIFFERENT subject, which this query was never
			// entitled to see in the first place.
			continue
		}
		out[c.Name] = c.Value
	}
	return out, nil
}

// SubjectsOfTypes returns every distinct subject of any of `types` that
// kindName has recorded a fact about within scope, sorted by (type, id).
//
// The counterpart to ForSubject: that answers "what is true of this object",
// this answers "which objects has anything been said about". A binder needs the
// second question because a fact is written keyed by its subject, so the set of
// subjects IS the set of instances an observation has proposed. Nothing else in
// the system enumerates them — a caller that only had ForSubject would have to
// already know the id it was looking for, which is precisely what it is trying
// to learn.
//
// It takes a LIST because its caller has a list: a class declares several slot
// types and must ask about all of them after every dispatch round, over the
// memory HTTP API in production. One query answers for the whole list, so the
// cost is flat in the slot count rather than one read per declared slot — and
// it stays flat in the overwhelming case where nothing has ever been observed.
//
// `types` is REQUIRED and non-empty. There is deliberately no "empty means
// every type" spelling: a caller that passed an unset variable would otherwise
// be handed subjects of every type in the session and could bind instances of
// types it never asked about. The filter stays structural — a type absent from
// the list never reaches the caller at all — which is what keeps a git_commit
// fact from proposing a candidate for a github_pr slot.
//
// The ids come back RAW, pre-transform — the value Record wrote, not the
// authz.ObjectID a slot's ValueTransforms would derive. See the package doc:
// a caller holding a transform chain runs it AFTER this, for the SpiceDB call
// only, and must never look up by the derived form.
//
// An empty result means nothing has been observed about any object of these
// types. It is not an error, and it must not be read as "there are none" in any
// stronger sense than that.
func SubjectsOfTypes(ctx context.Context, m memory.Memory, scope memory.Scope, kindName string, types []string) ([]Subject, error) {
	want := make(map[string]bool, len(types))
	for _, t := range types {
		if t != "" {
			want[t] = true
		}
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("factcontent.SubjectsOfTypes: no resource types named — this read must say which types it is asking about")
	}

	q := memory.Query{Scope: scope, Kinds: []string{kindName}}
	if len(want) == 1 {
		// A single type can use the Kind's declared index. Several cannot:
		// FieldEquals entries are AND-ed and there is no set/OR op, so the
		// predicate is omitted and the Go-side filter below does all the work.
		// That filter is authoritative in BOTH cases anyway (see its comment),
		// so this is purely an optimization and never a correctness input.
		q.FieldEquals = []memory.FieldFilter{{Path: "resourceType", Value: types[0]}}
	}
	res, err := m.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("factcontent.SubjectsOfTypes: query %s %v: %w", kindName, types, err)
	}
	if res.Truncated {
		// A truncated enumeration is a SHORTER candidate list, and a candidate
		// that never appears binds nothing — which looks exactly like a slot
		// nobody observed anything for. ForSubject can stay quiet about this
		// (it reads one subject's facts, and a missing fact already fails
		// closed as undetermined); an enumeration cannot, because the thing
		// that went missing is the very thing the caller was counting.
		slog.Info("factcontent.SubjectsOfTypes: the store held more matches than this read returned; some subjects are not proposed",
			"scope", scope.ID, "kind", kindName, "types", types, "returned", len(res.Entries))
	}

	seen := make(map[[2]string]bool, len(res.Entries))
	out := make([]Subject, 0, len(res.Entries))
	for _, e := range res.Entries {
		var c Content
		if err := json.Unmarshal(e.Content, &c); err != nil {
			// Loud for the same reason ForSubject is loud: an append-only fact
			// row that will not decode is corrupt or tampered, and a subject
			// vanishing from a binder's input is what the tamper-evident
			// subsystem exists to surface.
			undecodable.Skipped("factcontent.SubjectsOfTypes", scope, e, err)
			continue
		}
		// The authoritative filter, load-bearing exactly as in ForSubject. When
		// the predicate above was set, a backend that does not declare
		// Capabilities.ContentSchemas drops it and answers with the WHOLE Kind
		// in scope; when it was not set, the whole Kind is what was asked for.
		// Either way this is what confines the answer to the types the caller
		// named, so it is never redundant.
		key := [2]string{c.ResourceType, c.ResourceID}
		if !want[c.ResourceType] || c.ResourceID == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Subject{ResourceType: c.ResourceType, ResourceID: c.ResourceID})
	}
	// Sorted so a caller's per-candidate logs and grant writes come out in the
	// same order every run; Query makes no ordering promise across backends.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceType != out[j].ResourceType {
			return out[i].ResourceType < out[j].ResourceType
		}
		return out[i].ResourceID < out[j].ResourceID
	})
	return out, nil
}

// FactNames returns an observation's fact names, sorted.
//
// For LOG lines only, and the omission is the point: a fact's VALUE comes out
// of a signed payload or a tool result and can carry anything the upstream
// returned, so it does not belong in a log an operator greps. The names are
// what a reader needs to join a recorded observation against the precondition
// that reads it.
//
// Exported because both dispatch paths that record observed facts — the MCP
// dispatcher and the sandbox tool — need the same line, and an `observes` block
// is meant to behave identically across the two.
func FactNames(facts map[string]any) []string {
	out := make([]string, 0, len(facts))
	for name := range facts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
