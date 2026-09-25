// pkg/agent/runner/justification.go
//
// Harvest the agent's narration for the approval prompt: the last text block
// before the given tool_use_id, falling back to the most recent update_status
// text — agent prompts often discourage narration ("Do not narrate your tool
// calls") while encouraging update_status, which serves an approver just as well.
package runner

import (
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// LastStatusText returns the most recent update_status text observed
// across all assistant turns. Safe for concurrent access. Empty when
// no update_status has fired yet.
func (l *Loop) LastStatusText() string {
	l.lastStatusMu.RLock()
	defer l.lastStatusMu.RUnlock()
	return l.lastStatusText
}

// captureStatusFromUses scans `uses` for any update_status tool_use
// and stores the latest one on the Loop. Called once at the top of
// dispatchToolUses (synchronously, before goroutines spawn) so the
// cross-turn justification fallback in HarvestJustification sees the
// freshest narration regardless of in-turn use ordering.
func (l *Loop) captureStatusFromUses(uses []llm.ToolUseBlock) {
	var latest string
	for _, u := range uses {
		if u.Name != "update_status" {
			continue
		}
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(u.Input, &args); err == nil && args.Text != "" {
			latest = args.Text
		}
	}
	if latest == "" {
		return
	}
	l.lastStatusMu.Lock()
	l.lastStatusText = latest
	l.lastStatusMu.Unlock()
}

// HarvestJustification returns text describing why the agent is making the tool
// call with id == toolUseID — the string shown to the approver. Resolution
// order, highest priority first:
//
//  1. agentReason — the per-call `_reason` field on the tool call's wrapped
//     input envelope (pkg/agent/tool/errors.go: the schema requires
//     operation_id+_reason+args). Structured, authoritative, and always present
//     on gated MCP/sandbox tools.
//  2. The last `text` content block before the target tool_use in this turn.
//  3. The `text` arg of the most recent in-turn `update_status` preceding it.
//  4. priorStatusFallback — the newest update_status text from PRIOR turns (the
//     model often calls update_status, reads the result, then emits the gated
//     tool_use in a new turn).
//  5. Empty string.
//
// 2–4 are backstops for tool kinds that don't wrap their schema (meta tools) and
// for prompts that discourage narration.
func HarvestJustification(blocks []llm.ContentBlock, toolUseID string, agentReason string, priorStatusFallback string) string {
	// Primary: per-call _reason.
	if agentReason != "" {
		return agentReason
	}
	var lastText, lastStatus string
	// Seed the in-turn status accumulator with the cross-turn fallback.
	// Any in-turn update_status text will overwrite it (fresher wins).
	lastStatus = priorStatusFallback
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				lastText = b.Text
			}
		case "tool_use":
			if b.ToolUse == nil {
				continue
			}
			if b.ToolUse.ID == toolUseID {
				if lastText != "" {
					return lastText
				}
				return lastStatus
			}
			if b.ToolUse.Name == "update_status" {
				var args struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(b.ToolUse.Input, &args); err == nil && args.Text != "" {
					lastStatus = args.Text
				}
			}
		}
	}
	// Target tool_use wasn't found in this block list — return whatever
	// we accumulated. Same precedence as the in-loop return.
	if lastText != "" {
		return lastText
	}
	return lastStatus
}
