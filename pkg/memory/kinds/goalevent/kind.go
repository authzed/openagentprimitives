// Package goalevent registers the platform-owned, immutable goal transition log.
package goalevent

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const KindName = "goal_event"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "goalev-" }

// Audit snapshots contain retained source data; sessions read current goals
// through the goal API, which rechecks every source permission.
func (Kind) SessionReadable() bool                 { return false }
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true, EssentialWhileLive: true, NeverForkCopy: true, TTLAfterArchive: -1}
}

func (Kind) ContentSchema() reflect.Type                  { return reflect.TypeOf(goals.Event{}) }
func (Kind) IndexedFields() []string                      { return []string{"action"} }
func (Kind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(context.Context, memory.Signal) error { return nil }
func init()                                                 { memory.RegisterKind(Kind{}) }
