// Package toolcatalog is the memory Kind for the append-only record of which
// tools an agent was OFFERED, and from which turn.
//
// The transcript records what the model CALLED and never what it could have
// called, and those differ in the case that matters: a tool withheld by a
// capability gate or a plan-gate phase is invisible in a transcript, because
// not calling a tool you were never offered looks exactly like not calling a
// tool you declined. Steelthread capture needs the distinction to emit
// toolOffered / toolNotOffered; an investigator needs it to tell "the agent
// chose not to" from "the agent could not".
//
// One entry per CHANGE, not per turn. The offered set is constant across most
// of a session — per-turn writes would be pure bloat — but it genuinely moves
// when a phase is selected or a capability opens, and that movement is the
// whole point. Readers resolve a turn through ForTurn, which is a step
// function over the recorded changes.
package toolcatalog

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Content is one recorded catalog change.
type Content struct {
	// FromTurnIndex is the transcript index this catalog took effect at. It
	// stays in force until the next record.
	FromTurnIndex int `json:"fromTurnIndex"`
	// Digest is the hex sha256 of the sorted Tools, so a caller can compare two
	// catalogs without re-deriving the canonicalization.
	Digest string `json:"digest"`
	// Tools are the LLM-facing tool names, sorted.
	Tools []string `json:"tools"`
}

// KindName is the registered name of this memory Kind.
const KindName = "tool_catalog"

// IDPrefix is the entry-id prefix. The id embeds FromTurnIndex, which carries
// no content guarantee by itself — a conflict on it proves only that
// something is already recorded at that index. Record proves idempotency by
// comparing digests before treating a conflict as a no-op.
const IDPrefix = "toolcat-"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the runner records the catalog it actually offered.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
		// What an agent was permitted to reach for is evidence, on the same
		// footing as the instructions it ran under. A rewritten catalog would
		// erase the boundary it acted inside.
		AppendOnly: true,
	}
}

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"digest"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
