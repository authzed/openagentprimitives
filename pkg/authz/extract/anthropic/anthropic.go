// Package anthropic provides an extract.Provider backed by the
// shared llm.Provider seam. Defaults to Claude Haiku for low-latency
// extraction; the model is configurable per New() call.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
)

const (
	// DefaultModel is the cheapest fast model in the Anthropic family
	// for slice-3 extraction. Operators can override.
	DefaultModel = "claude-haiku-4-5-20251001"

	// ExtractionToolName is the synthetic tool name the LLM is forced to
	// call. Carries the structured array of extracted entities.
	ExtractionToolName = "extract_entities"

	// MaxOutputTokens caps the extraction LLM's output. Extractions are
	// small structured arrays; a generous cap keeps tool-use output JSON
	// from being truncated on edge cases (e.g., 50-entity dumps).
	MaxOutputTokens = 2048
)

// Provider is the Anthropic-backed entity extractor.
type Provider struct {
	llm   llm.Provider
	model string
}

// New constructs a Provider. model="" uses DefaultModel.
func New(llmProvider llm.Provider, model string) *Provider {
	if model == "" {
		model = DefaultModel
	}
	return &Provider{llm: llmProvider, model: model}
}

// Extract implements extract.Provider.
func (p *Provider) Extract(ctx context.Context, in extract.ExtractInput) ([]extract.ExtractedEntity, error) {
	if p.llm == nil {
		return nil, fmt.Errorf("authz/extract/anthropic: nil llm provider")
	}
	if len(in.EntityTypes) == 0 {
		return nil, nil
	}

	sysPrompt := buildSystemPrompt(in)
	tool := buildToolDef()

	req := llm.Request{
		Model:     p.model,
		System:    []llm.SystemBlock{{Text: sysPrompt}},
		Messages:  []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: in.UserMessage}}}},
		Tools:     []llm.ToolDef{tool},
		MaxTokens: MaxOutputTokens,
	}

	resp, err := p.llm.Send(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("authz/extract/anthropic: llm send: %w", err)
	}
	return decodeToolUse(resp), nil
}

// buildSystemPrompt assembles the system prompt for the extractor. The
// structure: entity-type list (with descriptions + aggregated per-tool
// prompts + class-level override), then already-bound list, then the
// instruction to emit a single extract_entities tool call.
func buildSystemPrompt(in extract.ExtractInput) string {
	var sb strings.Builder
	sb.WriteString("You are an entity-extraction step that runs before an agent processes the user's message.\n")
	sb.WriteString("Identify all entities of the declared types that the user mentions in the message.\n")
	sb.WriteString("Emit ONE call to extract_entities with the array of {resource_type, resource_id, source_text} for what you find.\n")
	sb.WriteString("Skip entities already bound to this session (see 'Already bound' below).\n\n")

	sb.WriteString("## Declared entity types\n\n")
	for _, t := range in.EntityTypes {
		fmt.Fprintf(&sb, "### %s\n%s\n", t.ResourceType, t.Description)
		// Class-level ExtractionPrompt overrides per-type aggregated prompts.
		if t.ExtractionPrompt != "" {
			sb.WriteString(t.ExtractionPrompt + "\n")
		} else if prompts, ok := in.PerToolPrompts[t.ResourceType]; ok {
			for _, p := range prompts {
				sb.WriteString(p + "\n")
			}
		}
		sb.WriteString("\n")
	}

	if len(in.AlreadyBound) > 0 {
		sb.WriteString("## Already bound\n\n")
		sb.WriteString("Do NOT re-extract these entities; they're already part of the session scope:\n")
		for _, b := range in.AlreadyBound {
			fmt.Fprintf(&sb, "- %s:%s\n", b.ResourceType, b.ResourceID)
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// buildToolDef builds the extract_entities tool the LLM is forced to call.
func buildToolDef() llm.ToolDef {
	schema := json.RawMessage(`{
        "type": "object",
        "required": ["entities"],
        "properties": {
            "entities": {
                "type": "array",
                "items": {
                    "type": "object",
                    "required": ["resource_type", "resource_id", "source_text"],
                    "properties": {
                        "resource_type": {"type": "string", "description": "One of the declared entity types"},
                        "resource_id":   {"type": "string", "description": "The raw resource ID extracted from the user message"},
                        "source_text":   {"type": "string", "description": "The <=200-char substring of the user message that produced this entity"},
                        "confidence":    {"type": "number", "description": "0..1 confidence; optional"}
                    }
                }
            }
        }
    }`)
	return llm.ToolDef{
		Name:        ExtractionToolName,
		Description: "Report the entities identified in the user message. Always call this exactly once.",
		InputSchema: schema,
	}
}

// decodeToolUse extracts entities from the LLM response. Robust to a
// missing tool_use (returns empty), to malformed JSON in tool_use args
// (returns empty + caller may log), and to confidence/source_text being
// absent.
func decodeToolUse(resp llm.Response) []extract.ExtractedEntity {
	for _, b := range resp.Content {
		if b.Type != "tool_use" || b.ToolUse == nil || b.ToolUse.Name != ExtractionToolName {
			continue
		}
		var payload struct {
			Entities []struct {
				ResourceType string  `json:"resource_type"`
				ResourceID   string  `json:"resource_id"`
				SourceText   string  `json:"source_text"`
				Confidence   float64 `json:"confidence"`
			} `json:"entities"`
		}
		if err := json.Unmarshal(b.ToolUse.Input, &payload); err != nil {
			return nil
		}
		out := make([]extract.ExtractedEntity, 0, len(payload.Entities))
		for _, e := range payload.Entities {
			if e.ResourceType == "" || e.ResourceID == "" {
				continue
			}
			out = append(out, extract.ExtractedEntity{
				ResourceType: e.ResourceType,
				ResourceID:   e.ResourceID,
				SourceText:   e.SourceText,
				Confidence:   e.Confidence,
			})
		}
		return out
	}
	return nil
}
