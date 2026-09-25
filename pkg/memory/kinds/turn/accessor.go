package turn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/internal/undecodable"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// turnContent is the JSON payload stored in Entry.Content. Index and role live
// in the Entry ID; CreatedAt is Entry.CreatedAt. The fields mirror memory.Turn,
// which documents them in full. The omitempty tags are load-bearing: the
// provenance digest omits empty fields, so an unset one must not perturb it.
type turnContent struct {
	// Content is the provider-neutral block list, persisted verbatim.
	Content []memory.ContentBlock `json:"content"`
	// Usage is token accounting; nil on anything but an assistant turn.
	Usage *memory.Usage `json:"usage,omitempty"`
	// Author is the human who wrote the turn; empty for agent-authored ones.
	Author identity.Subject `json:"author,omitempty"`
	// Via is the server-minted view URN of the surface that injected the turn.
	Via string `json:"via,omitempty"`
	// Refused marks an assistant turn the provider refused; replay skips it.
	Refused bool `json:"refused,omitempty"`
	// Model is "<provider>/<served-model>"; empty on user turns.
	Model string `json:"model,omitempty"`
}

// EntryID is the deterministic ID for a turn. The zero-padded index keeps
// lexicographic ID order aligned with (index, role) order.
func EntryID(index int, role string) string {
	return fmt.Sprintf("%s%06d-%s", IDPrefix, index, role)
}

// EntryToTurn reconstructs a Turn from its Entry. Index/role come from the ID;
// content/usage from Entry.Content. Exported so cmd/oap/internal/memstream can
// decode HTTP responses without duplicating the conversion.
func EntryToTurn(e memory.Entry) (memory.Turn, error) {
	var index int
	var role string
	if _, err := fmt.Sscanf(e.ID, IDPrefix+"%06d-%s", &index, &role); err != nil {
		return memory.Turn{}, fmt.Errorf("turn.EntryToTurn: bad ID %q: %w", e.ID, err)
	}
	var c turnContent
	if err := json.Unmarshal(e.Content, &c); err != nil {
		return memory.Turn{}, fmt.Errorf("turn.EntryToTurn: unmarshal %q: %w", e.ID, err)
	}
	return memory.Turn{
		Index: index, Role: role, Content: c.Content,
		Usage: c.Usage, CreatedAt: e.CreatedAt, Author: c.Author, Via: c.Via,
		Refused: c.Refused, Model: c.Model,
	}, nil
}

// Appender reads and writes turns for one session scope through a
// memory.Memory. It is the runner's transcript client and satisfies the
// loop's MemoryAppender structurally (ReadAll/ReadAfter/Append).
type Appender struct {
	mem   memory.Memory
	scope memory.Scope
}

func NewAppender(mem memory.Memory, scope memory.Scope) *Appender {
	return &Appender{mem: mem, scope: scope}
}

// Append writes t. Re-appending an identical (index, role) is a no-op;
// re-appending a different payload at the same (index, role) returns
// memory.ErrIndexConflict.
//
// Conflict detection is a best-effort check-then-put: the Query/Put pair is
// not atomic, so a TOCTOU window exists, acceptable only because a single
// writer per session scope (the runner loop) means no two turn-writers race.
// A failed conflict-check Query is fatal — returned rather than falling
// through to a blind Put, which would drop both the error and the check.
func (a *Appender) Append(ctx context.Context, t memory.Turn) error {
	id := EntryID(t.Index, t.Role)
	raw, err := json.Marshal(turnContent{Content: t.Content, Usage: t.Usage, Author: t.Author, Via: t.Via, Refused: t.Refused, Model: t.Model})
	if err != nil {
		return fmt.Errorf("turn.Append: marshal: %w", err)
	}
	existing, qerr := a.mem.Query(ctx, memory.Query{
		Scope: a.scope, Kinds: []string{KindName}, IDs: []string{id},
	})
	if qerr != nil {
		return fmt.Errorf("turn.Append: conflict check: %w", qerr)
	}
	if len(existing.Entries) == 1 {
		// Canonical JSON, not raw bytes: the existing entry came back OUT of a
		// backend, and postgres stores content in a JSONB column that re-emits
		// object keys in its own order and whitespace. A byte compare read every
		// legitimate re-append — a replay after restart, not only a retry after a
		// lost response — as divergent, failing ErrIndexConflict on postgres only.
		if bytes.Equal(memory.CanonicalContent(existing.Entries[0].Content), memory.CanonicalContent(raw)) {
			return nil // idempotent re-append
		}
		return memory.ErrIndexConflict
	}
	_, err = a.mem.Put(ctx, memory.Entry{
		Scope: a.scope, Kind: KindName, ID: id,
		CreatedAt: turnTime(t), Content: raw,
	})
	return err
}

func turnTime(t memory.Turn) time.Time {
	if t.CreatedAt.IsZero() {
		return time.Now().UTC()
	}
	return t.CreatedAt
}

// ReadAll returns every turn in the scope, ascending (index, role). An entry
// that will not decode is skipped and logged, not returned as an error — see
// below for why that is not a silent drop and why the alternative is fatal.
func (a *Appender) ReadAll(ctx context.Context) ([]memory.Turn, error) {
	res, err := a.mem.Query(ctx, memory.Query{Scope: a.scope, Kinds: []string{KindName}})
	if err != nil {
		return nil, err
	}
	turns := make([]memory.Turn, 0, len(res.Entries))
	for _, e := range res.Entries {
		tn, cerr := EntryToTurn(e)
		if cerr != nil {
			// Skipped, not fatal. turn is BOTH append-only and session-written,
			// so a session's own bearer may Put an entry whose ID merely starts
			// with "turn-": Memory.Put validates the Kind, the write authority
			// and the ID PREFIX, never the shape. Since EntryToTurn Sscanfs the
			// ID, an entry "turn-x" with EMPTY content is enough — and nothing
			// can then remove it, because per-entry Delete is refused for an
			// append-only Kind and DeleteScope skips it.
			//
			// A hard error here would therefore wedge EVERY read of that scope
			// forever, with no narrowing — no tag filters which entries reach
			// the decoder, so one row poisons the whole transcript. It wedges
			// four readers: the operator's restart/fork reconcile (which then
			// requeues forever), every restarted runner pod's replay, every
			// inbox drain, and the resumed browser transcript.
			//
			// Logged with the entry ID and its publisher because a skipped turn
			// is a GAP in a tamper-evident log, and naming the writer is the
			// only remedy left once the row cannot be deleted.
			undecodable.Skipped("turn.ReadAll", a.scope, e, cerr)
			continue
		}
		turns = append(turns, tn)
	}
	sort.Slice(turns, func(i, j int) bool {
		if turns[i].Index != turns[j].Index {
			return turns[i].Index < turns[j].Index
		}
		return turns[i].Role < turns[j].Role
	})
	return turns, nil
}

// ReadAfter returns turns with Index > after.
func (a *Appender) ReadAfter(ctx context.Context, after int) ([]memory.Turn, error) {
	all, err := a.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, t := range all {
		if t.Index > after {
			out = append(out, t)
		}
	}
	return out, nil
}

// ReadAll is a package-level convenience wrapper around NewAppender(m, scope).ReadAll.
// Callers that do not need the full Appender can use this directly.
func ReadAll(ctx context.Context, m memory.Memory, scope memory.Scope) ([]memory.Turn, error) {
	return NewAppender(m, scope).ReadAll(ctx)
}

// RecordInbox writes a Role:"inbox" turn at the given index, attributed to
// author (empty for non-human seeds). Mirrors channelsd's memory append;
// used by tests and channelsd pipelines that need to seed inbox turns
// without constructing a full Appender.
func RecordInbox(ctx context.Context, m memory.Memory, scope memory.Scope, idx int, text string, author identity.Subject) error {
	a := NewAppender(m, scope)
	return a.Append(ctx, memory.Turn{
		Index: idx, Role: "inbox",
		Content:   []memory.ContentBlock{{Type: "text", Text: text}},
		CreatedAt: time.Now().UTC(),
		Author:    author,
	})
}
