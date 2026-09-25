// Package tool_dispatch_snapshot implements the "tool_dispatch_snapshot"
// memory kind: a record that the runner requested — and the controller
// committed — a workspace snapshot immediately before a specific tool
// dispatch with stateImpact ∈ {readwrite, external}.
//
// One entry per stateful dispatch, linked to its turn (Relation="for_turn").
// The AgentSession restart reconciler reads these to map "cut at turn N" →
// "which snapshot to restore for which bundle PVC".
package tool_dispatch_snapshot

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "tool_dispatch_snapshot"
	IDPrefix = "tds-"
)

// Content is the JSON payload. There is no handle field: the snapshot is
// addressed by the (SessionUID, TurnIndex, Sequence) triple the Snapshotter's
// committed handle materializes, which is enough to find it on the store PVC.
type Content struct {
	// ToolUseID is the LLM-emitted tool_use ID this dispatch carried.
	ToolUseID string `json:"toolUseID"`

	// SpiceboxSession is the bundle session whose workspace PVC was
	// snapshotted. (Multi-bundle sessions snapshot per-bundle.)
	SpiceboxSession string `json:"spiceboxSession"`

	// TurnIndex is the turn this dispatch lived in.
	TurnIndex int `json:"turnIndex"`

	// Sequence is the within-turn ordering: 0 = first stateful
	// dispatch in this turn, 1 = second, etc.
	Sequence int `json:"sequence"`

	// SessionUID is the parent AgentSession's UID at dispatch time.
	// Snapshot handles are addressed by this UID + (TurnIndex, Seq).
	SessionUID string `json:"sessionUID"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the operator writes the snapshot in-process
// (internal/cmd/operator/main.go); the restart decision reads it.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		EssentialWhileLive: true,
		AppendOnly:         true, // tool dispatch snapshots capture the pre-call state for forensics — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
