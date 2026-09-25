// Package envelopefact is the memory Kind for a fact derived by platform code
// from a VERIFIED inbound delivery — the signed webhook body that opened or
// advanced a session.
//
// Separate from observedfact, and the separation is the security property, not
// packaging taste. WriteAuthority is per-Kind, so making this one
// ComponentWritten is what makes "this fact came from a signed provider
// payload" a claim the memory facade enforces rather than a field an author
// fills in. A runner holds a session token and is refused at the door.
package envelopefact

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// KindName is the registered name of this memory Kind. It is also the CEL
// namespace preconditions address these facts through (`facts.envelope.*`).
const KindName = "envelope_fact"

// IDPrefix is the entry-id prefix.
const IDPrefix = "envfact-"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return IDPrefix }

// WriteAuthority: channelsd owns inbound deliveries, as it owns
// channel_msg_ref and trigger_delivery. See the package comment for why this
// answer is load-bearing.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.ComponentWritten }

func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 30 * 24 * time.Hour,
		// A fact that governed what an agent was allowed to do is evidence,
		// and it must be write-once: see factcontent.EntryID.
		AppendOnly: true,
	}
}

func (Kind) ContentSchema() reflect.Type { return reflect.TypeOf(factcontent.Content{}) }

// IndexedFields: the read path asks "every fact about this subject", so both
// halves of the subject are indexed. Names are the Content struct's json tags,
// not its Go field names.
func (Kind) IndexedFields() []string { return []string{"resourceType", "resourceID"} }

func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
