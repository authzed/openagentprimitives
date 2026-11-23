package lifecycle

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

type hooks struct {
	scope memory.Scope
}

// signalIDInfix namespaces the entry IDs of recorded signals, for the same
// reason SignalTagPrefix namespaces their tags. The kind is caller-supplied and
// Append's event IDs are "lifecycle-evt-<wire type>-…", so an unnamespaced
// recording of a kind like "evt-stopped" would be indistinguishable from a
// transition event. Recordings are "lifecycle-sig-…", disjoint by construction.
const signalIDInfix = "sig-"

// OnSignal records a lifecycle Entry for the signal, tagged with the signal's
// kind inside the signal namespace (SignalTag). Any signal in the scope is
// recorded — the timeline falls out of every Kind's ScopeHooks fan-out —
// except memory.SignalEntryAppended, which is a framework-level signal
// announcing that some entry landed, not a session lifecycle event: recording
// it would make every append-only write (including this hook's own) produce a
// second append-only write.
//
// Every caller-controlled part of the signal is confined here, because this entry
// is signed as system:operator (see SendSignal's hand-off) and so carries the
// operator's authority: the kind reaches Tags only through SignalTag and the ID
// only through sanitize, both landing in namespaces no reader treats as
// authoritative. The payload stays opaque — recorded as the operator's testimony
// that this signal ARRIVED, never read back as a typed event.
func (h *hooks) OnSignal(ctx context.Context, sig memory.Signal) error {
	if sig.Kind == memory.SignalEntryAppended {
		return nil
	}
	memMu.RLock()
	m := memRef
	memMu.RUnlock()
	if m == nil {
		return nil
	}
	// Operator in-process caller, as the kg_ingestion hook also declares itself.
	// The facade authorized the SIGNAL before dispatching hooks, but the approval
	// it carries is the sender's — and m is the operator's signing facade, whose
	// append-only Put first READS the scope to seed the publisher's hash chain.
	// Without a system approval that read is denied, so the first lifecycle entry
	// per scope per operator process fails: harmless when some other operator-side
	// write seeds the chain first, fatal on any ordering (or restart) where none
	// does.
	ctx = memory.WithSystemApproval(ctx, "operator:lifecycle")
	at := sig.At
	if at.IsZero() {
		// Some senders omit event time. Stamp receipt time before signing:
		// SQLite's UnixNano encoding cannot round-trip Go's zero time.
		at = time.Now().UTC()
	}
	_, err := m.Put(ctx, memory.Entry{
		Scope:     sig.Scope,
		Kind:      Kind{}.Name(),
		ID:        Kind{}.IDPrefix() + signalIDInfix + sanitize(string(sig.Kind)) + "-" + randSuffix(),
		CreatedAt: at,
		Tags:      []string{SignalTag(sig.Kind)},
		Content:   sig.Payload,
	})
	return err
}

// maxIDSegment caps the derived segment of an entry ID. The signal kind that
// feeds it arrives from a request body, so without a cap a caller chooses the
// length of a field the operator signs and every backend indexes.
const maxIDSegment = 64

// sanitize reduces s to the characters an entry ID may safely carry — letters,
// digits, '-' and '_' — mapping everything else to '-' and capping the result at
// maxIDSegment bytes.
//
// An allowlist rather than a substitution list, because one of its two callers
// feeds it a CALLER-SUPPLIED signal kind: a newline, a control character or a
// megabyte of text would otherwise land verbatim inside an ID the operator signs.
// Its other caller (Append, on an event wire-type name) passes values already
// within the allowlist.
func sanitize(s string) string {
	if len(s) > maxIDSegment {
		s = s[:maxIDSegment]
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			out[i] = c
		default:
			out[i] = '-'
		}
	}
	return string(out)
}

func randSuffix() string {
	full := memory.NewID(Kind{})
	if len(full) <= len(Kind{}.IDPrefix()) {
		return full
	}
	return full[len(Kind{}.IDPrefix()):]
}
