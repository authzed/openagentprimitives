// Package observation is the memory Kind for a note an agent session writes
// into a resource-scoped memory pool: free text one session concluded about
// a resource (a customer, a repo, whatever the resource type is), durable
// for a later session holding a slot on the same pool to read.
//
// Content is deliberately minimal — the observation text and nothing else.
// A Kind's schema is hard to change once entries exist, so this carries only
// what the design calls for today. The "free-form tag set" the design asks
// for is not a Content field: Entry.Tags (pkg/memory/entry.go) and
// Query.Tags / SearchRequest.Tags already give every Kind an unordered,
// queryable tag set for free, and Entry.Links already gives every Kind typed
// references to other entries (the originating session, a superseded
// observation) — duplicating either inside Content would be exactly the
// speculative growth this package is told to avoid.
package observation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// KindName is the registered name of this memory Kind.
const KindName = "observation"

// IDPrefix is the entry-id prefix.
const IDPrefix = "obs-"

// Content is one observation's payload.
type Content struct {
	// Text is the observation itself — what the writing session concluded
	// about the resource, for a later session's own judgement to read.
	Text string `json:"text"`
}

// NewEntry builds one observation Entry ready to sign and store: Kind, a
// server-shaped ID (memory.NewID(Kind{})), CreatedAt stamped now, the
// caller's tags, and the marshaled Content. Scope is left unset — the
// caller's destination (session scope for an ordinary Put, a resource pool
// for PutToPool) is a property of WHERE the entry is written, not of what
// the entry IS.
//
// The one place this Kind's Entry gets assembled, for a reason stronger than
// convenience: EntryDigest signs ID and CreatedAt along with everything else
// (pkg/memory/provenance/digest.go), and an append-only entry that omits
// either signs a digest that can never re-verify once the server has to fill
// one in — httpsrv.putEntry assigns an ID when the body has none, and
// nothing anywhere assigns CreatedAt, so a caller that hand-built this Entry
// without going through NewEntry silently produced an entry that either
// fails verify-on-write (missing ID) or persists at the zero time forever
// (missing CreatedAt), neither of which any unit test catches unless its own
// fixture happens to reproduce EXACTLY what a production caller sends. Every
// writer of this Kind — record_observation, and anything that pins its
// shape in a test — calls this rather than hand-assembling the Entry, so the
// two cannot diverge on what "a production caller sends" means.
func NewEntry(text string, tags []string) (memory.Entry, error) {
	content, err := json.Marshal(Content{Text: text})
	if err != nil {
		return memory.Entry{}, fmt.Errorf("observation: marshal content: %w", err)
	}
	return memory.Entry{
		Kind:      KindName,
		ID:        memory.NewID(Kind{}),
		CreatedAt: time.Now().UTC(),
		Tags:      tags,
		Content:   content,
	}, nil
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the agent authors observations through its per-session
// memory bearer, the same way it authors a label or a turn. SessionWritten
// is a claim about the THREAT model, not convenience (memory.WriteAuthority's
// own doc, pkg/memory/kind.go): it says the session's agent loop — driven by
// model output and by whatever content its tools pulled in — is an
// acceptable author of this record.
//
// That is true here ONLY because nothing reads an observation to make an
// authorization decision. An observation is data in the untrusted envelope:
// a later session's own model reads it and forms its own judgement, exactly
// like any other tool result — it never gates a permission, a slot, or a
// write. If a future change makes a component read observations to decide
// something, this declaration must change to ComponentWritten (the zero
// value) before that component ships, not after.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

// Retention: append-only. A resource pool is multi-writer — every session
// holding a slot on the resource can write into it — so a mutable entry
// would let one session silently rewrite another's observation. That is not
// merely an integrity problem: it is an influence vector, since rewriting
// what a past agent concluded steers every future agent that reads the pool,
// with no trace left behind. Append-only, plus the existing per-(scope,
// publisher) provenance chain, makes that structurally impossible rather
// than merely discouraged. A correction is a new entry linking `supersedes`
// to the one it corrects, never an edit to it.
func (Kind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true}
}

func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(Content{}) }

// IndexedFields: none. Text is free-form prose, not a structured predicate —
// filtering an observation pool is by Tags or Links, both already indexed by
// the framework itself, not by a content field.
func (Kind) IndexedFields() []string { return nil }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
