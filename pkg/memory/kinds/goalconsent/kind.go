// Package goalconsent stores the platform's exact consent card and the signed
// human decision. Neither record is agent-readable or agent-writable.
package goalconsent

import (
	"context"
	"encoding/json"
	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"reflect"
)

const KindName = "goal_consent"

type Content struct {
	Goal           goals.Goal      `json:"goal"`
	Request        json.RawMessage `json:"request"`
	Owner          string          `json:"owner,omitempty"`
	Approved       *bool           `json:"approved,omitempty"`
	RequestWitness json.RawMessage `json:"requestWitness,omitempty"`
}
type Kind struct{}

func (Kind) Name() string                          { return KindName }
func (Kind) IDPrefix() string                      { return "goalconsent-" }
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }
func (Kind) SessionReadable() bool                 { return false }
func (Kind) Retention() memory.Retention {
	return memory.Retention{AppendOnly: true, EssentialWhileLive: true, NeverForkCopy: true, TTLAfterArchive: -1}
}
func (Kind) ContentSchema() reflect.Type                  { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                      { return nil }
func (Kind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(context.Context, memory.Signal) error { return nil }
func init()                                                 { memory.RegisterKind(Kind{}) }

// Find retrieves one platform consent record through the memory facade.
func Find(ctx context.Context, mem memory.Memory, scope memory.Scope, id string) (memory.Entry, bool, error) {
	rows, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{KindName}, IDs: []string{id}, Limit: 1})
	if err != nil {
		return memory.Entry{}, false, err
	}
	if len(rows.Entries) == 0 {
		return memory.Entry{}, false, nil
	}
	return rows.Entries[0], true, nil
}
