package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

const (
	DefaultExtractorModel = "claude-haiku-4-5-20251001"
	ExtractorToolName     = "emit_scope_delta"
	ExtractorMaxTokens    = 2048
)

// AnthropicExtractor is the production Extractor. Wraps an llm.Provider
// so test seams (fake llm.Provider) can isolate the LLM round-trip.
type AnthropicExtractor struct {
	llm   llm.Provider
	model string
}

// NewAnthropicExtractor constructs an AnthropicExtractor. model="" uses
// DefaultExtractorModel.
func NewAnthropicExtractor(p llm.Provider, model string) *AnthropicExtractor {
	if model == "" {
		model = DefaultExtractorModel
	}
	return &AnthropicExtractor{llm: p, model: model}
}

// Extract implements Extractor. This is the only LLM call that sees raw
// user text (the liquid-input boundary).
func (a *AnthropicExtractor) Extract(ctx context.Context, in ExtractorInput) (scope.ScopeDelta, error) {
	if a.llm == nil {
		return scope.ScopeDelta{}, fmt.Errorf("authzd/metaagent: nil llm provider")
	}
	sys := buildExtractorSystemPrompt(in)
	tool := buildExtractorTool()

	resp, err := a.llm.Send(ctx, llm.Request{
		Model:     a.model,
		MaxTokens: ExtractorMaxTokens,
		System:    []llm.SystemBlock{{Text: sys}},
		Messages:  []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: in.UserRequest}}}},
		Tools:     []llm.ToolDef{tool},
	})
	if err != nil {
		return scope.ScopeDelta{}, fmt.Errorf("authzd/metaagent: extractor LLM call: %w", err)
	}

	// Find the tool_use block.
	for _, blk := range resp.Content {
		if blk.Type != "tool_use" || blk.ToolUse == nil {
			continue
		}
		if blk.ToolUse.Name != ExtractorToolName {
			continue
		}
		var d scope.ScopeDelta
		if err := json.Unmarshal(blk.ToolUse.Input, &d); err != nil {
			return scope.ScopeDelta{}, fmt.Errorf("authzd/metaagent: decode delta: %w", err)
		}
		if err := validateExtractedDelta(d); err != nil {
			return scope.ScopeDelta{}, err
		}
		return d, nil
	}
	// LLM didn't call the tool — treat as empty (CannotAddress path).
	return scope.ScopeDelta{}, nil
}

func buildExtractorSystemPrompt(in ExtractorInput) string {
	envBytes, _ := json.MarshalIndent(in.AgentClassEnvelope, "", "  ")
	scopeBytes, _ := json.MarshalIndent(in.CurrentScope, "", "  ")
	disBytes, _ := json.MarshalIndent(in.CurrentDisallows, "", "  ")
	accessBytes, _ := json.MarshalIndent(in.RequesterAccessHints, "", "  ")
	return fmt.Sprintf(`You are the metaagent's extractor. Translate the user's permission-scope request into a structured ScopeDelta. You MUST call the %s tool with the structured delta — never reply with free-form text.

If the user request is unclear, contradictory, or doesn't map to a scope change, call %s with an empty delta: {}.

The ScopeDelta has three axes:
  - "add": resources, tools, resourcePatterns, argConstraints to widen scope into
  - "remove": items to narrow scope by
  - "hardDeny": items to permanently deny in this session (writes SpiceDB tuples)

Requester: %s
Current time: %s

AgentClass envelope:
%s

Current session scope:
%s

Current SpiceDB explicit-disallows:
%s

Requester's SpiceDB-accessible resources (hint):
%s
`, ExtractorToolName, ExtractorToolName, in.Requester, time.Now().UTC().Format(time.RFC3339), envBytes, scopeBytes, disBytes, accessBytes)
}

func buildExtractorTool() llm.ToolDef {
	// Schema permissive on the inner structure — the LLM emits JSON
	// matching scope.ScopeDelta's tags. authzd validates downstream.
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"add":      { "type": "object" },
			"remove":   { "type": "object" },
			"hardDeny": { "type": "object" }
		},
		"additionalProperties": false
	}`)
	return llm.ToolDef{
		Name:        ExtractorToolName,
		Description: "Emit a structured ScopeDelta describing how to change the session's permission scope. Always call this exactly once.",
		InputSchema: schema,
	}
}

func validateExtractedDelta(d scope.ScopeDelta) error {
	all := append(append([]scope.ResourceRef{}, d.Add.Resources...), d.HardDeny.Resources...)
	for _, ref := range all {
		if ref.ResourceType == "" {
			return fmt.Errorf("authzd/metaagent: extracted resource missing resourceType")
		}
		if ref.ID == "" {
			return fmt.Errorf("authzd/metaagent: extracted resource missing id (type=%q)", ref.ResourceType)
		}
	}
	return nil
}
