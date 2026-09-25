// Package contentguardaudit is the memory Kind for the audit log of
// content-guard enforcement: per-call content-inspection findings
// (pass/block/approve), one entry per firing. Queried via `oap memory query`.
package contentguardaudit

import (
	"context"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// Content is one content-guard finding.
type Content struct {
	Inspector string `json:"inspector"` // the inspector ID, e.g. "url-allowlist"
	Action    string `json:"action"`    // pass|block|approve
	// Tool is the LLM tool name whose input or output was inspected.
	Tool  string `json:"tool"`
	Point string `json:"point"` // pre_tool_call|post_tool_call
	// UseID is the tool_use block id this finding belongs to. NOT POPULATED:
	// contentguard.Event carries no use id, so the sole writer never sets it.
	UseID string `json:"useID,omitempty"`
	// Reason is the inspector's human-readable explanation; empty on a pass.
	Reason  string `json:"reason,omitempty"`
	Details string `json:"details,omitempty"` // JSON of the finding details (e.g. offending URLs + rule)
	// Provenance records where the inspected content came from. NOT POPULATED,
	// for the same reason as UseID — despite the tag lacking omitempty, so it
	// serializes as "" on every entry.
	Provenance string `json:"provenance"`
	// At is when the inspection ran; defaulted to now by Record.
	At time.Time `json:"at"`
}

// KindName is the registered name of this memory Kind.
const KindName = "contentguard_audit"

type Kind struct{}

func (Kind) Name() string     { return KindName }
func (Kind) IDPrefix() string { return "cgaud-" }

// WriteAuthority: the runner records its own content-guard verdicts
// (pipeline_wiring.go).
func (Kind) WriteAuthority() memory.WriteAuthority { return memory.SessionWritten }
func (Kind) Retention() memory.Retention {
	return memory.Retention{
		ArchiveOn:       []memory.SignalKind{lifecycle.SigSessionCompleted, lifecycle.SigSessionFailed},
		TTLAfterArchive: 90 * 24 * time.Hour,
		AppendOnly:      true, // content-guard findings are security audit evidence — write-once.
	}
}
func (Kind) ContentSchema() reflect.Type                    { return reflect.TypeOf(Content{}) }
func (Kind) IndexedFields() []string                        { return []string{"at", "tool", "action"} }
func (Kind) NewScopeHooks(_ memory.Scope) memory.ScopeHooks { return hooks{} }

type hooks struct{}

func (hooks) OnSignal(_ context.Context, _ memory.Signal) error { return nil }

func init() { memory.RegisterKind(Kind{}) }
