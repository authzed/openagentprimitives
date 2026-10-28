// Package replydelivery stores accepted replies in the private browser
// transcript. Acceptance and its receipt are one append-only write.
package replydelivery

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

const KindName = "reply_delivery"
const IDPrefix = "replydel-"

type Content struct {
	Closed  bool             `json:"closed,omitempty"`
	Intent  delivery.Intent  `json:"intent"`
	Receipt delivery.Receipt `json:"receipt"`
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
