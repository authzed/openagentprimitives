// Package approval is the memory Kind for approval-flow events
// (request + outcome). Request entries are written when the runner
// publishes an interaction_request for a decision category (tool_approval,
// info_leakage, …); outcome entries are written when channelsd reports back
// the user's decision. Each links to the originating tool call by toolUseID.
package approval

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

type Request struct {
	// RequestID is the channel-visible request id (the interaction RequestRef);
	// tagged request:<id> so RequestByID recovers this entry after the
	// in-memory cache is gone — used by the generic Show-Details flow and by
	// the decision pipe's cross-restart resource recovery.
	RequestID string `json:"requestID,omitempty"`
	// ToolName is the LLM tool name whose call is awaiting a decision.
	ToolName string `json:"toolName"`
	// Approver is the canonical subject the request was addressed to; empty
	// when the approver is derived from Resources' owners at decision time.
	Approver string `json:"approver"`
	// Reason is the agent's stated justification for asking, shown to the
	// approver; empty when the request carried none.
	Reason string `json:"reason,omitempty"`
	// Resources are the source resources whose #owner may decide, recovered
	// by the decision pipe on a cache miss for cross-restart owner
	// resolution.
	Resources []channelevents.InteractionResourceRef `json:"resources,omitempty"`
	// Details is the category-specific interaction details (json-encoded),
	// read by the generic Show-Details modal and the grant-write handler
	// after the in-memory cache is gone.
	Details json.RawMessage `json:"details,omitempty"`
}

type Outcome struct {
	Decision string `json:"decision"` // "approved" | "denied"
	// Approver is the canonical subject who actually answered — not
	// necessarily the Request's Approver, which may have been owner-derived.
	Approver string `json:"approver"`
	// Reason is the approver's stated rationale; empty when they gave none.
	Reason string `json:"reason,omitempty"`
}

// KindName is the registered name of this memory Kind.
const KindName = "approval"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "approval-" }

// WriteAuthority: the runner records its own tool-call approval requests and
// outcomes (pkg/agent/runner/host_approval.go).
func (Kind) WriteAuthority() memory.WriteAuthority          { return memory.SessionWritten }
func (Kind) Retention() memory.Retention                    { return memory.Retention{AppendOnly: true} } // approval requests and outcomes are audit evidence — write-once.
func (Kind) ContentSchema() reflect.Type                    { return nil }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
