package runner

import "sync"

// PinDriftState tracks tools whose backing dependency drifted from its pin
// baseline while the effective mode escalates them to per-call approval.
// Concurrent: written at session start / tool refresh, read per dispatch.
type PinDriftState struct {
	mu     sync.RWMutex
	byTool map[string]string // LLM tool name -> human-readable drift summary
}

// NewPinDriftState returns an initialised PinDriftState.
func NewPinDriftState() *PinDriftState {
	return &PinDriftState{byTool: map[string]string{}}
}

// MarkDrifted records that the given tool's backing dependency has drifted.
// summary is the human-readable description surfaced in the approval ask.
func (p *PinDriftState) MarkDrifted(toolName, summary string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byTool[toolName] = summary
}

// Drifted reports whether toolName is currently marked as drifted and, if so,
// returns the summary recorded by the most recent MarkDrifted call.
func (p *PinDriftState) Drifted(toolName string) (summary string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	summary, ok = p.byTool[toolName]
	return summary, ok
}

// Clear removes a tool from the drifted set. Used when the tool set is
// refreshed and the new observation matches the baseline again.
func (p *PinDriftState) Clear(toolName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byTool, toolName)
}
