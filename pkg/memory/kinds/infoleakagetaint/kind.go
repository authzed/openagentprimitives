// Package infoleakagetaint is the memory Kind for per-session taint records.
// Each entry captures one resource read via a tool call so later gates can
// answer "what resources flowed into this session's context?"
package infoleakagetaint

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

const KindName = "infoleakage_taint"

// TaintRecord is the per-tool-call record of a resource read into context.
type TaintRecord struct {
	// ToolUseID is the tool_use block whose result carried the resource into
	// context. Also written as the entry's for_tool_call link.
	ToolUseID string `json:"toolUseID"`
	// AccessedAt is when the read happened; defaulted to now by Append and used
	// as the entry's CreatedAt.
	AccessedAt time.Time `json:"accessedAt"`
	// ResourceType is the SpiceDB object type that entered context.
	ResourceType string `json:"resourceType"`
	// ResourceID is the SpiceDB object id that entered context. (Type, ID) is
	// what the respond-time audience check re-authorizes every recipient on.
	ResourceID string `json:"resourceID"`
	// Permission is the permission the read was authorized under, recorded so
	// the audience check knows which grant a recipient would need.
	Permission string `json:"permission"`
	// ToolName is the MCP tool that accessed the resource. Populated by the
	// post-execute leakage hook so the info-leakage approval UX can show
	// reviewers which tool was called ("via bash_run", "via hubspot_get_contact").
	ToolName string `json:"toolName,omitempty"`
}

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "ilt-" }

// WriteAuthority: the runner marks content its own tool reads tainted
// (internal/cmd/runner/main.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:  []memory.SignalKind{lifecycle.SigSessionCompleted},
		AppendOnly: true, // info-leakage taint markers are provenance evidence — write-once.
		// TTLAfterArchive zero: taint records live as long as the archived session.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(TaintRecord{}) }
func (Kind) IndexedFields() []string                        { return nil }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
