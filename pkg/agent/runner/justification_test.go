package runner_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/runner"
)

// TestHarvestJustification covers the harvest helper: it returns the
// most recent text block emitted strictly before the named tool_use,
// regardless of intervening unrelated tool_uses, and returns "" when
// there is no preceding text.
func TestHarvestJustification(t *testing.T) {
	cases := []struct {
		name                string
		blocks              []llm.ContentBlock
		toolUse             string
		agentReason         string
		priorStatusFallback string
		want                string
	}{
		{
			name: "text before tool_use: returns nearest preceding text",
			blocks: []llm.ContentBlock{
				{Type: "text", Text: "first thought"},
				{Type: "text", Text: "second thought — opening the PR"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1", Name: "gh_pr_create"}},
			},
			toolUse: "tu-1",
			want:    "second thought — opening the PR",
		},
		{
			name: "no preceding text: returns empty",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse: "tu-1",
			want:    "",
		},
		{
			name: "text after tool_use ignored: only earlier text counts",
			blocks: []llm.ContentBlock{
				{Type: "text", Text: "before"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
				{Type: "text", Text: "after — should not appear"},
			},
			toolUse: "tu-1",
			want:    "before",
		},
		{
			// Sanity: harvest returns the most recent text *anywhere* before
			// the target tool_use, even if other tool_uses intervene. (The
			// model is allowed to interleave thinking and unrelated calls.)
			name: "intervening unrelated tool_use: returns nearest preceding text",
			blocks: []llm.ContentBlock{
				{Type: "text", Text: "explaining call A"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-A"}},
				{Type: "text", Text: "explaining call B"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-B"}},
			},
			toolUse: "tu-B",
			want:    "explaining call B",
		},
		{
			// Fallback path: no text block before the target, but an
			// update_status tool_use precedes it. Its `text` arg becomes
			// the justification — agent prompts that discourage narration
			// still produce useful approver context this way.
			name: "no preceding text, update_status precedes: returns status text",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID:    "tu-status",
					Name:  "update_status",
					Input: json.RawMessage(`{"text":"Searching contacts on Acme so I can attribute the company.","short":"Searching Acme contacts"}`),
				}},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1", Name: "hubspot_search_crm_objects"}},
			},
			toolUse: "tu-1",
			want:    "Searching contacts on Acme so I can attribute the company.",
		},
		{
			// Precedence: an explicit text block ALWAYS wins over an
			// update_status text. The text block is the model's deliberate
			// channel for justification; status is the fallback.
			name: "text block + update_status both present: text wins",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID:    "tu-status",
					Name:  "update_status",
					Input: json.RawMessage(`{"text":"status text"}`),
				}},
				{Type: "text", Text: "deliberate justification"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse: "tu-1",
			want:    "deliberate justification",
		},
		{
			// Multiple status updates before a tool call (model spam):
			// return the most recent one.
			name: "multiple update_status calls: most recent wins",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu-s1", Name: "update_status",
					Input: json.RawMessage(`{"text":"first status"}`),
				}},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu-s2", Name: "update_status",
					Input: json.RawMessage(`{"text":"second status"}`),
				}},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse: "tu-1",
			want:    "second status",
		},
		{
			// Defensive: update_status with malformed Input shouldn't
			// crash or pollute the justification.
			name: "update_status with bad JSON input: ignored gracefully",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu-s", Name: "update_status",
					Input: json.RawMessage(`{not valid json`),
				}},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse: "tu-1",
			want:    "",
		},
		{
			// Per-call _reason: the agent-supplied reason on the tool
			// call's own envelope wins over EVERY other source. This
			// is the structured per-call justification path; the
			// other sources are fallbacks for when it's missing.
			name: "agentReason wins over text block and update_status",
			blocks: []llm.ContentBlock{
				{Type: "text", Text: "stale narration that should be ignored"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu-s", Name: "update_status",
					Input: json.RawMessage(`{"text":"also stale"}`),
				}},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse:             "tu-1",
			agentReason:         "Searching Acme contacts so I can attribute the company to its owner.",
			priorStatusFallback: "cross-turn fallback",
			want:                "Searching Acme contacts so I can attribute the company to its owner.",
		},
		{
			// Cross-turn fallback: production case where the model
			// called update_status in turn N and emitted the gated
			// tool_use in turn N+1. The current turn's blocks contain
			// ONLY the tool_use (no text, no update_status), so the
			// in-turn fallback paths above all fail; the caller-supplied
			// priorStatusFallback is the only signal available. AND
			// agentReason is empty (defensive — some tools may not
			// wrap their input with the operation_id/_reason envelope).
			name: "no in-turn narration + empty agentReason: priorStatusFallback wins",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse:             "tu-1",
			priorStatusFallback: "Searching contacts on Acme",
			want:                "Searching contacts on Acme",
		},
		{
			// Precedence: an in-turn update_status MUST beat the cross-
			// turn fallback. The fresher narration is more relevant.
			name: "in-turn update_status beats priorStatusFallback",
			blocks: []llm.ContentBlock{
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
					ID: "tu-s", Name: "update_status",
					Input: json.RawMessage(`{"text":"fresher in-turn status"}`),
				}},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse:             "tu-1",
			priorStatusFallback: "older cross-turn status",
			want:                "fresher in-turn status",
		},
		{
			// Precedence: a text block STILL beats both update_status
			// paths (in-turn and cross-turn).
			name: "text block beats priorStatusFallback",
			blocks: []llm.ContentBlock{
				{Type: "text", Text: "deliberate justification"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu-1"}},
			},
			toolUse:             "tu-1",
			priorStatusFallback: "older cross-turn status",
			want:                "deliberate justification",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, runner.HarvestJustification(tc.blocks, tc.toolUse, tc.agentReason, tc.priorStatusFallback))
		})
	}
}
