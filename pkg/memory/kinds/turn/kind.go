// Package turn registers the LLM-transcript Kind. Each Entry is one
// memory.Turn; the deterministic ID turn-<index>-<role> makes re-append
// idempotent and (index, role) order recoverable.
package turn

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const (
	KindName = "turn"
	IDPrefix = "turn-"
)

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the transcript is the agent session authoring its own
// conversation.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		EssentialWhileLive: true,
		AppendOnly:         true, // turns are the LLM transcript — audit evidence, written once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return nil }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
