// Package pttagcontent is the memory Kind for redacted content held behind a
// provenance tag.
//
// When a datum is too sensitive to place in the model's context, the context
// carries the tag and the bytes live here. The platform can resolve them — to
// forward the datum to a destination that IS authorized for it, or to answer an
// audit — and the agent cannot.
//
// It exists as durable storage because the prototype's content store was an
// in-memory map. Sessions sleep, are reaped, and rehydrate; a map is gone by
// then, and every tag minted before the restart would resolve to nothing while
// still appearing to be tracked. That is the durability lens's exact failure:
// state whose loss is silent and unrecoverable, in front of no durable record.
package pttagcontent

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const KindName = "pt_tag_content"

// ContentRecord is the redacted bytes behind one tag.
type ContentRecord struct {
	// TagID is the pt_tag whose audience governs this content.
	TagID string `json:"tagID"`
	// Content is the datum the model was not shown.
	Content string `json:"content"`
	// MIME describes Content, so a platform forwarding it can label it.
	MIME string `json:"mime,omitempty"`
	// StoredAt is when the content was set aside; used as the entry's CreatedAt.
	StoredAt time.Time `json:"storedAt"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "ptc-" }

// WriteAuthority is component-written, for the same reason pt_tag itself is:
// the record is an input to a disclosure decision, and a session that could
// author it could author the content a later check reasons about.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

// SessionReadable is FALSE, and this Kind is the reason the read door exists.
//
// The whole point of holding content here is that the model does not see it.
// Without a read door the agent could ask for it straight back through
// query_memory — the tool takes arbitrary Kind names — and redaction would be
// a formality: the content would be absent from the context window and one
// tool call away.
//
// Note this is not defence against a hostile model so much as against an
// ordinary one. An agent told "you cannot see the document" and given a
// general-purpose memory query will reasonably try the query.
func (Kind) SessionReadable() bool { return false }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn: []memory.SignalKind{lifecycle.SigSessionCompleted},
		// Append-only: the content is what a disclosure decision was made
		// about. A mutable record would let the bytes change after the check
		// that cleared them, with the audit trail still pointing here.
		AppendOnly: true,
	}
}

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(ContentRecord{}) }
func (Kind) IndexedFields() []string                        { return []string{"tagID"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
