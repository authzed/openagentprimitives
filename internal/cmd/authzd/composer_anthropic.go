package main

import (
	"context"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

const (
	DefaultComposerModel = "claude-haiku-4-5-20251001"
	ComposerToolName     = "emit_approver_prose"
	ComposerMaxTokens    = 1024

	fallbackApproverSummary = "Approval requested for a scope change. Click Show Details for the technical view."
)

// AnthropicComposer renders user-facing prose from structured classification.
// CRITICAL: this LLM call sees NO raw user text — its system prompt and
// messages are built only from authzd-curated structured data.
type AnthropicComposer struct {
	llm   llm.Provider
	model string
}

// NewAnthropicComposer constructs an AnthropicComposer. model="" uses
// DefaultComposerModel.
func NewAnthropicComposer(p llm.Provider, model string) *AnthropicComposer {
	if model == "" {
		model = DefaultComposerModel
	}
	return &AnthropicComposer{llm: p, model: model}
}

// Compose implements Composer.
// CRITICAL: ComposerInput carries NO raw user text. The prompt-injection
// boundary is enforced by the field shape of ComposerInput — no sanitization
// step can be relied upon; the field simply doesn't exist.
func (c *AnthropicComposer) Compose(ctx context.Context, in ComposerInput) (ComposerOutput, error) {
	if c.llm == nil {
		return fallbackComposerOutput(), nil
	}
	sys := composerSystemPrompt()
	// Marshal the structured input as JSON; this is the entire LLM-visible input.
	// NO field of ComposerInput carries raw user text — the prompt-injection
	// boundary is enforced by the field shape, not by sanitization.
	structuredJSON, err := json.MarshalIndent(map[string]any{
		"applied":                in.Applied,
		"skipped":                in.Skipped,
		"caveats":                in.Caveats,
		"toolDescriptions":       in.ToolDescriptions,
		"resourceTypeHumanNames": in.ResourceTypeHumanNames,
	}, "", "  ")
	if err != nil {
		return fallbackComposerOutput(), nil
	}

	resp, err := c.llm.Send(ctx, llm.Request{
		Model:     c.model,
		MaxTokens: ComposerMaxTokens,
		System:    []llm.SystemBlock{{Text: sys}},
		Messages:  []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: string(structuredJSON)}}}},
		Tools:     []llm.ToolDef{composerTool()},
	})
	if err != nil {
		return fallbackComposerOutput(), nil
	}

	for _, blk := range resp.Content {
		if blk.Type != "tool_use" || blk.ToolUse == nil || blk.ToolUse.Name != ComposerToolName {
			continue
		}
		var out ComposerOutput
		if err := json.Unmarshal(blk.ToolUse.Input, &out); err != nil {
			return fallbackComposerOutput(), nil
		}
		if out.ApproverSummary == "" {
			out.ApproverSummary = fallbackApproverSummary
		}
		return out, nil
	}
	return fallbackComposerOutput(), nil
}

func fallbackComposerOutput() ComposerOutput {
	return ComposerOutput{ApproverSummary: fallbackApproverSummary}
}

func composerSystemPrompt() string {
	return `You are the metaagent's response composer. Compose plain-language prose for a non-technical approver. You MUST call emit_approver_prose with the prose; never reply with free-form text.

Rules for the prose:
1. Never use internal identifiers in snake_case or technical jargon (no "resource_type:id", no SpiceDB, no JSON, no permission names like "read"). Use the human names supplied in resourceTypeHumanNames.
2. approverSummary: ONE sentence, conditional ("If you approve, the agent will be able to / will be blocked from / ..."). Lead with the most important effect.
3. skippedExplanations: one short explanation per Skipped item; reference the human name; suggest a constructive next action when one exists.
4. caveatExplanations: one short explanation per Caveat item; open with the limitation in 5-10 words, then a short sentence on what it means in practice.
5. Never invent facts beyond what the structured input contains.
`
}

func composerTool() llm.ToolDef {
	return llm.ToolDef{
		Name:        ComposerToolName,
		Description: "Emit the approver-facing prose: summary and per-item explanations. Always call this exactly once.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"approverSummary":     { "type": "string" },
				"skippedExplanations": { "type": "array", "items": { "type": "string" } },
				"caveatExplanations":  { "type": "array", "items": { "type": "string" } }
			},
			"required": ["approverSummary"]
		}`),
	}
}
