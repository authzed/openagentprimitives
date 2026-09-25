// Package uiviewmodel is the memory Kind for an agent-UI Tier-1 view-model:
// one record per (session, ui, hook), rewritten in place each time the agent
// calls update_view for that hook. It is the durable half of "the agent
// modifies the UI" — a page reloaded, a session slept and rehydrated, or a
// second viewer opening the same session all resolve the same hooks.
package uiviewmodel

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const (
	KindName = "ui_view_model"
	IDPrefix = "uivm-"
)

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// Retention:
//
//   - Mutable, NOT append-only. Every update_view for a given (ui, slot)
//     REPLACES the previous record; that is the Kind's entire purpose, and an
//     append-only Kind would reject the second write with
//     ErrAppendOnlyConflict, letting the agent compose a slot exactly once.
//     Not an audit gap: the mutation is already audited, because update_view
//     is an ordinary tool call in the transcript, so an append-only projection
//     would bloat the chain to record nothing new.
//   - EssentialWhileLive, so the state survives idle-sleep, reap and rehydrate.
//     A live session's composed UI must not be evicted by a soft per-scope cap
//     — a chatty session would silently lose the agent's work and repaint
//     Tier 0 with nothing in any log.
//   - ArchiveOn the same session-completion signals the sibling ui_action Kind
//     uses, with a matching TTLAfterArchive: a completed session's page stays
//     readable for as long as its action records do, so a viewer who opens it
//     after the fact does not see half a UI.
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		EssentialWhileLive: true,
		ArchiveOn:          []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive:    24 * time.Hour,
	}
}

// WriteAuthority: the agent authors its own Tier-1 view-model through the
// runner's per-session memory bearer — update_view reaches here via
// uiview.Runtime.Write. That is the Kind's entire purpose, so a component-only
// classification would refuse every legitimate write.
//
// Session-authored is also the right THREAT answer, not merely the convenient
// one: the record is model-driven content that no component reads to make an
// authorization decision. It is rendered as Tier-1 — untrusted, revalidated
// against the closed uicomponents vocabulary on the way out — so a compromised
// runner writing hostile view-models buys no capability it did not already
// have by calling update_view directly.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"ui", "slot"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

// memory.RegisterKind panics at process start on any IDPrefix collision or
// substring overlap, so importing this package at all proves "uivm-" is free.
func init() { memory.RegisterKind(Kind{}) }
