package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

const (
	DefaultColdStartExtractorModel = "claude-haiku-4-5-20251001"
	ColdStartExtractorToolName     = "emit_cold_start_extraction"
	ColdStartExtractorMaxTokens    = 2048
)

// ColdStartExtractor parses the first user message of a new scope-enabled
// session into a ColdStartExtraction (scope delta + cleaned task).
type ColdStartExtractor struct {
	llm   llm.Provider
	model string
}

func NewColdStartExtractor(p llm.Provider, model string) *ColdStartExtractor {
	if model == "" {
		model = DefaultColdStartExtractorModel
	}
	return &ColdStartExtractor{llm: p, model: model}
}

func (e *ColdStartExtractor) ExtractColdStart(ctx context.Context, in ExtractorInput) (scope.ColdStartExtraction, error) {
	if e.llm == nil {
		return scope.ColdStartExtraction{}, fmt.Errorf("authzd: nil llm provider for cold-start extractor")
	}
	resp, err := e.llm.Send(ctx, llm.Request{
		Model:     e.model,
		MaxTokens: ColdStartExtractorMaxTokens,
		System:    []llm.SystemBlock{{Text: buildColdStartSystemPrompt(in)}},
		Messages:  []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: in.UserRequest}}}},
		Tools:     []llm.ToolDef{coldStartTool()},
	})
	if err != nil {
		return scope.ColdStartExtraction{}, fmt.Errorf("authzd: cold-start extractor LLM: %w", err)
	}
	for _, blk := range resp.Content {
		if blk.Type != "tool_use" || blk.ToolUse == nil || blk.ToolUse.Name != ColdStartExtractorToolName {
			continue
		}
		var out scope.ColdStartExtraction
		if err := json.Unmarshal(blk.ToolUse.Input, &out); err != nil {
			return scope.ColdStartExtraction{}, fmt.Errorf("authzd: decode cold-start extraction: %w", err)
		}
		return out, nil
	}
	return scope.ColdStartExtraction{}, nil
}

func buildColdStartSystemPrompt(in ExtractorInput) string {
	envBytes, _ := json.MarshalIndent(in.AgentClassEnvelope, "", "  ")
	return fmt.Sprintf(`You are the metaagent's cold-start extractor. The user's first message may contain BOTH a task for the agent AND permission-scope instructions ("only", "do not", "allow", "disallow", "restrict to", etc.). Call %s exactly once with:
  - scopeDelta: the permission changes the user asked for, grouped under add / remove / hardDeny. Each of those is an OBJECT (never an array) with optional arrays: resources [{resourceType,id}], resourcePatterns [{attrs:{key:glob}}], tools [name], argConstraints. Omit any axis with no change; omit scopeDelta entirely if there are none.
  - cleanedTask: the user's message with the permission-setting language REMOVED — the literal task the agent should perform. The agent CANNOT set its own permissions, so strip that language. If the message is purely permission-setting with no task, cleanedTask MUST be an empty string.

Use resourceTypes that appear in the AgentClass envelope below. To block a
WHOLE CLASS of resources (e.g. "all issues under team ENG"), hard-deny the
specific resource type with an id GLOB, NOT the container/parent object — the
deny is matched against the id of each resource the agent actually accesses.
Linear issue ids are "<TEAM_KEY>-<number>" (e.g. ENG-369), so "issues under
ENG" is the glob "ENG-*" on linear_issue. Use a concrete id to block one
resource. Globs use shell syntax (*, ?).

Example: "summarize linear issue L-140 and do not read any issues in ENG"
  -> scopeDelta = {"hardDeny": {"resources": [{"resourceType": "linear_issue", "id": "ENG-*"}]}}
     cleanedTask = "summarize linear issue L-140"
  (hardDeny is an object whose "resources" is an array — NOT itself an array.
   Deny the linear_issue type with the "ENG-*" glob, not linear_team:ENG, because
   the agent reads linear_issue:ENG-369, not the team object.)

AgentClass envelope (the outer bound on what scope can ever be):
%s
`, ColdStartExtractorToolName, envBytes)
}

func coldStartTool() llm.ToolDef {
	// scopeDelta MUST decode into scope.ScopeDelta: each of add/remove/hardDeny
	// is a ScopePartial OBJECT (never an array). A bare {"type":"object"} let the
	// model emit hardDeny as an array, which failed to unmarshal. We describe the
	// full ScopePartial shape and inline it for each axis (no $ref — provider
	// support for $ref in tool input_schema is inconsistent).
	partial := map[string]any{
		"type":        "object",
		"description": "Scope changes grouped by axis. ALL fields are arrays; this object is NEVER itself an array.",
		"properties": map[string]any{
			"resources": map[string]any{
				"type":        "array",
				"description": "Concrete resources, each {resourceType,id}.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"resourceType": map[string]any{"type": "string"},
						"id":           map[string]any{"type": "string"},
					},
					"required":             []string{"resourceType", "id"},
					"additionalProperties": false,
				},
			},
			"resourcePatterns": map[string]any{
				"type":        "array",
				"description": "Glob predicates over resource attributes, each {attrs:{key:glob}}.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"attrs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
					},
					"additionalProperties": false,
				},
			},
			"tools": map[string]any{
				"type":        "array",
				"description": "Tool names to add/remove/deny.",
				"items":       map[string]any{"type": "string"},
			},
			"argConstraints": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"tool":   map[string]any{"type": "string"},
						"equals": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"forbid": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"cel":    map[string]any{"type": "string"},
					},
					"required":             []string{"tool"},
					"additionalProperties": false,
				},
			},
		},
		"additionalProperties": false,
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scopeDelta": map[string]any{
				"type":        "object",
				"description": "Permission changes. Each axis is an object; omit axes with no change.",
				"properties": map[string]any{
					"add":      partial,
					"remove":   partial,
					"hardDeny": partial,
				},
				"additionalProperties": false,
			},
			"cleanedTask": map[string]any{"type": "string"},
		},
		"required": []string{"cleanedTask"},
	}
	schemaBytes, _ := json.Marshal(schema)
	return llm.ToolDef{
		Name:        ColdStartExtractorToolName,
		Description: "Emit the parsed scope delta and the cleaned agent task from the user's first message.",
		InputSchema: schemaBytes,
	}
}
