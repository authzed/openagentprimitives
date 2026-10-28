// Package interactionhistory stores display snapshots, not authorization grants.
// Requests and resolutions remain separate append-only records so a refresh or
// restart can reproduce the card the human saw and its exact outcome.
package interactionhistory

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const KindName = "interaction_history"
const IDPrefix = "ihist-"

type Content struct {
	Source    channelevents.SessionRef                 `json:"source"`
	SourceUID string                                   `json:"sourceUID"`
	At        time.Time                                `json:"at"`
	Request   *channelevents.InteractionRequestPayload `json:"request,omitempty"`
	Applied   *channelevents.InteractionAppliedPayload `json:"applied,omitempty"`
}

type Kind struct{}

func (Kind) Name() string                          { return KindName }
func (Kind) IDPrefix() string                      { return IDPrefix }
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{EssentialWhileLive: true, AppendOnly: true}
}

func (Kind) ContentSchema() reflect.Type                  { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                      { return nil }
func (Kind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(context.Context, memory.Signal) error { return nil }
func init()                                                 { memory.RegisterKind(Kind{}) }
