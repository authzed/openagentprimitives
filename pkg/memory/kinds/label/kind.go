// Package label is the memory Kind for (resourceType, id) → display
// label tuples extracted from MCP tool responses. Source is
// attacker-controllable; every Entry carries trust:untrusted so
// downstream consumers (channel renderers especially) refuse to feed
// these values to any LLM.
package label

import (
	"context"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

const SoftCap = 4096 // matches the in-process LabelStore's DefaultLabelStoreCap

type Label struct {
	// ResourceType is the SpiceDB object type the label describes; with
	// ResourceID it forms the deterministic entry ID, so a re-Record for the
	// same pair overwrites.
	ResourceType string `json:"resourceType"`
	// ResourceID is the SpiceDB object id the label describes.
	ResourceID string `json:"resourceId"`
	// Label is the display string lifted out of an MCP tool response.
	// ATTACKER-CONTROLLED — every entry carries trust:untrusted, and no
	// consumer may feed this value to an LLM.
	Label string `json:"label"`
}

// KindName is the registered name of this memory Kind.
const KindName = "label"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "label-" }

// WriteAuthority: the runner caches resource labels it resolved during its own
// dispatches.
func (Kind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (Kind) Retention() memory.Retention                    { return memory.Retention{SoftCapPerScope: SoftCap} }
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Label{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
