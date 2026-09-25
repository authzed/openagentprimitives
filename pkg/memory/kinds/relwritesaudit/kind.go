// Package relwritesaudit is the memory Kind for tuples written by
// pkg/authz/relwrites after a successful tool call. Each Audit entry
// captures the tuples emitted in one Run, so operators can answer "what
// tuples did this session generate?" without grepping SpiceDB logs.
package relwritesaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Tuple is one SpiceDB relationship as it was written.
type Tuple struct {
	// Resource is the object the relation was written on, as "type:id".
	Resource string `json:"resource"`
	// Relation is the relation name written between Resource and Subject.
	Relation string `json:"relation"`
	// Subject is the grantee, as "type:id" with an optional "#relation".
	Subject string `json:"subject"`
}

// Audit is the tuples one relwrites Run emitted, and what produced them.
type Audit struct {
	// Tuples is every relationship the run wrote, in the order written.
	Tuples []Tuple `json:"tuples"`
	// Source is the agent-facing tool name whose call produced the writes.
	Source string `json:"source"`
}

// KindName is the registered name of this memory Kind.
const KindName = "relwrites_audit"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "relw-" }

// WriteAuthority: the runner records the relationship writes its own MCP
// dispatches performed.
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted},
		TTLAfterArchive: 30 * 24 * time.Hour,
		AppendOnly:      true, // relationship-write audit records are tamper-evident evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Audit{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
