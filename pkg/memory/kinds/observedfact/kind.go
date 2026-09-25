// Package observedfact is the memory Kind for a fact derived from a tool
// RESULT by a declared `observes` block.
//
// Its trust grade is lower than envelopefact's and deliberately visible in the
// name: the agent chose the arguments of the call that produced this, so an
// author gating on a signed-envelope fact must not silently receive one of
// these. That distinction is enforced by the Kind split, not by a field —
// see the envelopefact package comment.
package observedfact

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// KindName is the registered name of this memory Kind. It is also the CEL
// namespace preconditions address these facts through (`facts.observed.*`).
const KindName = "observed_fact"

// IDPrefix is the entry-id prefix.
const IDPrefix = "obsfact-"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: the runner records facts from its own dispatches, the same
// answer relwrites_audit gives for the same reason.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
		AppendOnly:      true,
	}
}

func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(factcontent.Content{}) }
func (Kind) IndexedFields() []string     { return []string{"resourceType", "resourceID"} }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
