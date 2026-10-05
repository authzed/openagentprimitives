// Package sessionobservation stores platform-authored event observations. Agent
// credentials cannot manufacture verified event evidence by writing this kind.
package sessionobservation

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const KindName = "session_observation"
const IDPrefix = "sessobs-"

type Content struct {
	Observation sessionevents.Observation `json:"observation"`
}

type Kind struct{}

func (Kind) Name() string                          { return KindName }
func (Kind) IDPrefix() string                      { return IDPrefix }
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) SessionReadable() bool                 { return false }
func (Kind) Retention() memory.Retention {
	return memory.Retention{EssentialWhileLive: true, AppendOnly: true, NeverForkCopy: true, TTLAfterArchive: -1}
}

func (Kind) ContentSchema() reflect.Type                  { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                      { return nil }
func (Kind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(context.Context, memory.Signal) error { return nil }
func init()                                                 { memory.RegisterKind(Kind{}) }
