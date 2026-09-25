package runner

import (
	"sync"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ProvenanceRecord carries pin-origin facts for one synthesized tool.
// It is stored in ToolProvenance, keyed by LLM tool name.
type ProvenanceRecord struct {
	// Pin is the observed PinRecord for the dependency backing this tool
	// (MCP manifest hash, image digest, …).
	Pin spiceboxv1alpha1.PinRecord

	// Name is the dependency's declared name (MCPServer ref name,
	// SidecarToolbox ref name, toolkit name, canonical skill name, …).
	// Used as PinName on authzdecision audit records so an operator can
	// correlate the decision with the specific dependency CRD.
	Name string

	// DriftSummary is non-empty when the dependency drifted from its
	// recorded pin baseline. Its presence drives the audit record's boolean
	// PinDrifted field (true iff DriftSummary != ""); empty means no drift
	// observed.
	DriftSummary string

	// BypassReason is non-empty when a pinning-policy bypass suppressed
	// an otherwise-applicable rule for this dependency. Copied into the
	// audit record so operators can see why a drifted tool was still allowed.
	BypassReason string
}

// ToolProvenance is a concurrent map of LLM tool name → ProvenanceRecord.
// It is written once at session start (during tool synthesis) and read
// concurrently per tool call (by recordAuthzDecision). The zero value is
// NOT usable; construct via NewToolProvenance.
type ToolProvenance struct {
	mu     sync.RWMutex
	byTool map[string]ProvenanceRecord // LLM tool name → ProvenanceRecord
}

// NewToolProvenance returns an initialised ToolProvenance.
func NewToolProvenance() *ToolProvenance {
	return &ToolProvenance{byTool: map[string]ProvenanceRecord{}}
}

// Set records the provenance for one tool. Overwrites any prior entry for
// the same toolName (last write wins; only one entry per tool is expected).
func (p *ToolProvenance) Set(toolName string, rec ProvenanceRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byTool[toolName] = rec
}

// Get returns the ProvenanceRecord for toolName and true if one was recorded,
// or a zero ProvenanceRecord and false when no record exists.
func (p *ToolProvenance) Get(toolName string) (ProvenanceRecord, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	rec, ok := p.byTool[toolName]
	return rec, ok
}
