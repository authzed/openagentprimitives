// Package uiaction is the memory Kind for an agent-UI action's lifecycle
// record: one entry per request ID, rewritten in place as the call
// progresses (submitted -> awaiting_approval -> running -> settled). A
// browser reads this back over livemirror so a reload or a ten-minute
// approval wait never loses the outcome.
package uiaction

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const (
	KindName = "ui_action"
	IDPrefix = "uiact-"
)

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// Retention:
//
//   - Mutable, NOT append-only. Every write for a request ID REPLACES the
//     previous one as the lifecycle advances, which is the point of the Kind;
//     append-only would reject the second write with ErrAppendOnlyConflict.
//     The audit story is unaffected — the underlying tool call is already in
//     the tamper-evident authz-decision / tool-session chains.
//   - EssentialWhileLive, so the record always outlives the resolved approval
//     timeout. A soft per-scope cap evicting FIFO would let a click-happy
//     dashboard evict its own pending approval record out from under itself;
//     this makes that unrepresentable rather than a matter of tuning a number.
//   - ArchiveOn the same session-completion signals the sibling Kinds use,
//     with TTLAfterArchive comfortably above the default 10m
//     AuthzBlock.ResolvedApprovalTimeout, for the one case EssentialWhileLive
//     doesn't cover: a session that completes with an approval still pending.
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		EssentialWhileLive: true,
		ArchiveOn:          []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive:    24 * time.Hour,
	}
}

// WriteAuthority: the runner records its own UI-action lifecycle transitions
// (pkg/agent/runner/uiactionrecord.go), on the same per-session bearer it uses
// for the sibling `approval` Kind that this one parallels for the browser.
//
// It is a DISPLAY projection, never an authorization input: the browser's
// authority to run an action is re-decided per call by handleAppToolCallReq's
// interact re-auth and readonly gate, which read the server-side declaration
// and never this record. So a session that forges a State here changes what a
// viewer is shown, not what it may do.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"requestId", "requester", "state"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

// memory.RegisterKind panics at process start on any IDPrefix collision or
// substring overlap, so importing this package at all proves "uiact-" is free.
func init() { memory.RegisterKind(Kind{}) }
