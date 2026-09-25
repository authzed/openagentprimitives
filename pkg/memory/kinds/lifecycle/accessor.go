package lifecycle

import (
	"context"
	"fmt"
	"time"

	lc "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// EventTag marks a lifecycle entry as a typed transition event written by a
// sequencer (operator or runner), distinguishing it from the signal recordings
// the ScopeHooks also write to this kind. Folding the transition stream reads
// only entries carrying this tag — a recorded signal payload is not an Event
// envelope and must not be fed to Unmarshal.
const EventTag = "lifecycle.event"

// SignalTagPrefix namespaces every tag the ScopeHooks derives from a signal's
// kind, and SignalTag is the only constructor for one.
//
// The kind is CALLER-SUPPLIED — httpsrv's handleSignal takes Kind, At and
// Payload from the request body, and Local.SendSignal strips the token session
// at its authorship hand-off so the hook writes as the operator. A kind copied
// verbatim into Tags therefore let a session bearer choose a tag on an entry
// signed system:operator, and choosing EventTag minted operator testimony about
// a transition that never happened — which the fold below reads and `oap audit
// verify` validates.
//
// The prefix makes the two namespaces DISJOINT: every tag this hook can emit
// starts with it, and nothing this package reserves (EventTag, and marshal.go's
// typeNameOf wire names) does. No input can produce a reserved tag, and there is
// no reserved-name list to keep in step as the wire types grow.
const SignalTagPrefix = "signal:"

// SignalTag returns the tag under which the ScopeHooks records a signal of this
// kind. Read the recorded-signal stream back through it rather than through the
// bare kind string — the raw kind is not a tag any entry carries.
func SignalTag(k memory.SignalKind) string { return SignalTagPrefix + string(k) }

// Timeline returns every lifecycle entry in scope, ordered oldest-
// first. Returns (nil, nil) when scope has no lifecycle entries.
func Timeline(ctx context.Context, m memory.Memory, scope memory.Scope) ([]memory.Entry, error) {
	res, err := m.Query(ctx, memory.Query{
		Scope:   scope,
		Kinds:   []string{Kind{}.Name()},
		OrderBy: memory.OrderBy{Field: "createdAt"},
	})
	if err != nil {
		return nil, err
	}
	return res.Entries, nil
}

// Append writes a typed transition event to the scope's append-only lifecycle
// log. The entry is tagged with EventTag (so Events can read it back) plus the
// event's stable wire type, and carries key — the cross-publisher fold-ordering
// key — inside the signed Content. A zero key (OrderKey{}) writes the legacy
// unstamped form, which folds by createdAt. m must be a signing facade for the
// append-only kind — the caller (the sequencer) holds the per-publisher signer.
func Append(ctx context.Context, m memory.Memory, scope memory.Scope, ev lc.Event, at time.Time, key OrderKey) error {
	payload, err := MarshalWithOrder(ev, key)
	if err != nil {
		return err
	}
	typ, err := typeNameOf(ev)
	if err != nil {
		return err
	}
	_, err = m.Put(ctx, memory.Entry{
		Scope:     scope,
		Kind:      Kind{}.Name(),
		ID:        Kind{}.IDPrefix() + "evt-" + sanitize(typ) + "-" + randSuffix(),
		CreatedAt: at,
		Tags:      []string{EventTag, typ},
		Content:   payload,
	})
	if err != nil {
		return fmt.Errorf("lifecycle.Append: put %s: %w", typ, err)
	}
	return nil
}

// Events reads the typed transition events from scope in fold order — by the
// skew-immune OrderKey, not wall-clock createdAt (see order.go). Recorded signal
// entries (no EventTag) are skipped — only the transition stream is returned,
// ready to feed lifecycle.Fold. An entry tagged as an event that fails to decode
// is skipped and logged; see ReadOrdered for why that is not a hard error.
func Events(ctx context.Context, m memory.Memory, scope memory.Scope) ([]lc.Event, error) {
	ordered, err := ReadOrdered(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	return justEvents(ordered), nil
}

// EventsForIncarnation is Events narrowed to the AgentSession instance
// sessionUID: the events a previous instance wrote into this same scope are left
// out of the returned stream. Fold "this session's state" through this, not
// Events — a scope is shared by every AgentSession that has ever held its
// namespace/name, and the entries in it are permanent. See ForIncarnation.
func EventsForIncarnation(ctx context.Context, m memory.Memory, scope memory.Scope, sessionUID string) ([]lc.Event, error) {
	ordered, err := ReadOrdered(ctx, m, scope)
	if err != nil {
		return nil, err
	}
	return justEvents(ForIncarnation(ordered, sessionUID)), nil
}

// justEvents reduces an ordered log to the bare event stream Fold consumes.
func justEvents(ordered []OrderedEvent) []lc.Event {
	out := make([]lc.Event, len(ordered))
	for i := range ordered {
		out[i] = ordered[i].Event
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
