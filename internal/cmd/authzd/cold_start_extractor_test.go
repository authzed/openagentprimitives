package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func llmResponseText(s string) llm.Response {
	return llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: s}}}
}

func TestColdStartExtractor_Mixed(t *testing.T) {
	out := scope.ColdStartExtraction{
		ScopeDelta:  scope.ScopeDelta{HardDeny: scope.ScopePartial{ResourcePatterns: []scope.ResourcePattern{{Attrs: map[string]string{"team": "ENG"}}}}},
		CleanedTask: "summarize linear issue L-140",
	}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ColdStartExtractorToolName, out)}
	ext := NewColdStartExtractor(llmFake, "")
	got, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "summarize L-140 and do not read ENG issues"})
	require.NoError(t, err)
	assert.Equal(t, "summarize linear issue L-140", got.CleanedTask)
	assert.False(t, got.ScopeDelta.IsEmpty())
}

func TestColdStartExtractor_TaskOnly_EmptyDelta(t *testing.T) {
	out := scope.ColdStartExtraction{CleanedTask: "summarize L-140"}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ColdStartExtractorToolName, out)}
	ext := NewColdStartExtractor(llmFake, "")
	got, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "summarize L-140"})
	require.NoError(t, err)
	assert.True(t, got.ScopeDelta.IsEmpty())
	assert.Equal(t, "summarize L-140", got.CleanedTask)
}

func TestColdStartExtractor_ScopeOnly_EmptyTask(t *testing.T) {
	out := scope.ColdStartExtraction{ScopeDelta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"github.create_pr"}}}}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ColdStartExtractorToolName, out)}
	ext := NewColdStartExtractor(llmFake, "")
	got, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "do not open PRs"})
	require.NoError(t, err)
	assert.Empty(t, got.CleanedTask)
	assert.False(t, got.ScopeDelta.IsEmpty())
}

func TestColdStartExtractor_NoToolCall_EmptyExtraction(t *testing.T) {
	llmFake := &fakeLLM{resp: llmResponseText("I cannot help")}
	ext := NewColdStartExtractor(llmFake, "")
	got, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "x"})
	require.NoError(t, err)
	assert.True(t, got.ScopeDelta.IsEmpty())
	assert.Empty(t, got.CleanedTask)
}

// toolUseRawResponse builds a tool_use response from raw JSON input, so a test
// can feed the EXACT bytes a model might emit (not a marshaled Go struct, which
// is always well-shaped). This is how we reproduce the production decode failure.
func toolUseRawResponse(name, rawInput string) llm.Response {
	return llm.Response{Content: []llm.ContentBlock{{
		Type:    "tool_use",
		ToolUse: &llm.ToolUseBlock{Name: name, Input: []byte(rawInput)},
	}}}
}

// TestColdStartExtractor_HardDenyObject_Decodes is the regression test for the
// production bug where the model emitted scopeDelta.hardDeny as a JSON ARRAY and
// the decode failed ("cannot unmarshal array into ScopePartial"). The tightened
// tool schema steers the model to emit hardDeny as an OBJECT; this asserts the
// correct object shape decodes into ScopePartial.
func TestColdStartExtractor_HardDenyObject_Decodes(t *testing.T) {
	raw := `{"scopeDelta":{"hardDeny":{"resources":[{"resourceType":"linear_team","id":"ENG"}]}},"cleanedTask":"summarize linear issue L-140"}`
	ext := NewColdStartExtractor(&fakeLLM{resp: toolUseRawResponse(ColdStartExtractorToolName, raw)}, "")
	got, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "summarize L-140 and do not read ENG"})
	require.NoError(t, err, "well-formed object hardDeny must decode")
	assert.Equal(t, "summarize linear issue L-140", got.CleanedTask)
	require.Len(t, got.ScopeDelta.HardDeny.Resources, 1)
	assert.Equal(t, "linear_team", got.ScopeDelta.HardDeny.Resources[0].ResourceType)
	assert.Equal(t, "ENG", got.ScopeDelta.HardDeny.Resources[0].ID)
}

// TestColdStartExtractor_ToolSchema_HardDenyIsObject guards the schema itself:
// scopeDelta.{add,remove,hardDeny} must each be declared type:"object" with the
// ScopePartial array fields — NOT a bare object (which let the model emit an
// array and broke decoding).
func TestColdStartExtractor_ToolSchema_HardDenyIsObject(t *testing.T) {
	var schema struct {
		Properties struct {
			ScopeDelta struct {
				Properties struct {
					HardDeny struct {
						Type       string `json:"type"`
						Properties struct {
							Resources struct {
								Type string `json:"type"`
							} `json:"resources"`
						} `json:"properties"`
					} `json:"hardDeny"`
				} `json:"properties"`
			} `json:"scopeDelta"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(coldStartTool().InputSchema, &schema), "tool schema must be valid JSON")
	hd := schema.Properties.ScopeDelta.Properties.HardDeny
	assert.Equal(t, "object", hd.Type, "hardDeny must be an object, not free-form/array")
	assert.Equal(t, "array", hd.Properties.Resources.Type, "hardDeny.resources must be an array of refs")
}

func TestColdStartExtractor_LLMError(t *testing.T) {
	ext := NewColdStartExtractor(&fakeLLM{err: errors.New("boom")}, "")
	_, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "x"})
	require.Error(t, err)
}

func TestColdStartExtractor_NilLLM(t *testing.T) {
	ext := NewColdStartExtractor(nil, "")
	_, err := ext.ExtractColdStart(context.Background(), ExtractorInput{UserRequest: "x"})
	require.Error(t, err)
}
