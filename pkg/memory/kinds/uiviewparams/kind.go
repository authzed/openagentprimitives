// Package uiviewparams is the memory Kind for the agent's own binding-parameter
// choices on an agent-defined UI: one record per (session, ui), rewritten in
// place each time the agent calls set_view_params.
//
// A sibling of uiviewmodel, not a part of it: a view-model record says what a
// slot CONTAINS, while a params record says what the page's own controls are
// SET TO. That is not content at all — the author's declaration stands exactly
// as written, and only the values its bindings resolve with change.
//
// Durability is the whole reason this is stored rather than pushed and
// forgotten. A viewer who says "last seven days" in a channel and opens the
// browser an hour later must find the dashboard on that window. The live push
// reaches whoever is already watching; this record reaches everyone else,
// including the same person on their next page load.
package uiviewparams

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const (
	KindName = "ui_view_params"
	IDPrefix = "uivp-"
)

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// Retention mirrors uiviewmodel's, for the same reasons and one of its own:
//
//   - Mutable, NOT append-only. Each set_view_params REPLACES the previous
//     record; append-only would let the agent set the page's controls exactly
//     once. The mutation is already audited — the tool call is an ordinary
//     transcript entry — so an append-only projection would record nothing the
//     chain does not already hold.
//   - EssentialWhileLive, so a chatty session cannot evict the window its own
//     dashboard is showing and silently repaint it at the author's defaults.
//   - Archived and expired on the same signals and timeline as uiviewmodel: a
//     completed session's page must not come back with its slots intact and its
//     filters reset, showing real content under a window nobody chose.
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		EssentialWhileLive: true,
		ArchiveOn:          []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive:    24 * time.Hour,
	}
}

// WriteAuthority is session-authored: the agent chooses these values, through
// the runner's per-session memory bearer, which is the Kind's entire purpose.
//
// It is also the right THREAT answer. The stored values are model-influenced
// text and every reader treats them as such: uiview.Runtime admits only keys
// the CURRENT declaration's own controls drive (uicomponents.ParamKeys), and
// the browser reconciles them against the declaration it is actually rendering
// before any reaches a binding request. A forged or stale key is therefore
// inert — it names a control that does not exist, and is dropped.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"ui"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

// memory.RegisterKind panics at process start on any IDPrefix collision or
// substring overlap, so importing this package at all proves "uivp-" is free —
// including against uiviewmodel's neighbouring "uivm-".
func init() { memory.RegisterKind(Kind{}) }
