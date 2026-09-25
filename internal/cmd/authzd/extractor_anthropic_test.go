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

// fakeLLM is a scriptable llm.Provider for the extractor/composer tests.
type fakeLLM struct {
	resp llm.Response
	err  error
	seen llm.Request
}

func (f *fakeLLM) Name() string                            { return "fake" }
func (f *fakeLLM) SupportedFromEnv() bool                  { return true }
func (f *fakeLLM) Pricing(string) (llm.ModelPricing, bool) { return llm.ModelPricing{}, false }
func (f *fakeLLM) Capabilities(string) llm.CapabilitySet   { return llm.NewCapabilitySet() }
func (f *fakeLLM) NativeInputMIMEs(string) llm.MIMESet     { return nil }
func (f *fakeLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	f.seen = req
	if f.err != nil {
		return llm.Response{}, f.err
	}
	return f.resp, nil
}

func toolUseResponse(t *testing.T, name string, input any) llm.Response {
	t.Helper()
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	return llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use",
			ToolUse: &llm.ToolUseBlock{
				Name:  name,
				Input: raw,
			},
		}},
	}
}

func TestAnthropicExtractor_HappyPath_Add(t *testing.T) {
	delta := scope.ScopeDelta{
		Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}},
	}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ExtractorToolName, delta)}
	ext := NewAnthropicExtractor(llmFake, "")
	got, err := ext.Extract(context.Background(), ExtractorInput{UserRequest: "@metaagent allow foo/bar"})
	require.NoError(t, err)
	assert.Equal(t, delta, got)
	// System prompt includes the user request verbatim only in the user message,
	// not the system (it's the messages[0].content[0].text).
	require.Len(t, llmFake.seen.Messages, 1)
	require.Len(t, llmFake.seen.Messages[0].Content, 1)
	assert.Equal(t, "@metaagent allow foo/bar", llmFake.seen.Messages[0].Content[0].Text)
}

func TestAnthropicExtractor_HappyPath_HardDeny(t *testing.T) {
	delta := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1234"}}},
	}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ExtractorToolName, delta)}
	ext := NewAnthropicExtractor(llmFake, "")
	got, err := ext.Extract(context.Background(), ExtractorInput{UserRequest: "@metaagent disallow L-1234"})
	require.NoError(t, err)
	assert.Equal(t, delta, got)
}

func TestAnthropicExtractor_NoToolCall_ReturnsEmptyDelta(t *testing.T) {
	// LLM returns text instead of calling the tool — treat as empty delta.
	llmFake := &fakeLLM{resp: llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "I cannot help"}}}}
	ext := NewAnthropicExtractor(llmFake, "")
	got, err := ext.Extract(context.Background(), ExtractorInput{UserRequest: "x"})
	require.NoError(t, err)
	assert.True(t, got.IsEmpty())
}

func TestAnthropicExtractor_LLMError(t *testing.T) {
	llmFake := &fakeLLM{err: errors.New("rate limited")}
	ext := NewAnthropicExtractor(llmFake, "")
	_, err := ext.Extract(context.Background(), ExtractorInput{UserRequest: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limited")
}

func TestAnthropicExtractor_InvalidDeltaShape(t *testing.T) {
	// Extracted delta has a resource without resourceType — must reject.
	delta := scope.ScopeDelta{
		Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ID: "no-type"}}},
	}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ExtractorToolName, delta)}
	ext := NewAnthropicExtractor(llmFake, "")
	_, err := ext.Extract(context.Background(), ExtractorInput{UserRequest: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resourceType")
}

func TestAnthropicExtractor_NilLLMProvider(t *testing.T) {
	ext := NewAnthropicExtractor(nil, "")
	_, err := ext.Extract(context.Background(), ExtractorInput{UserRequest: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil llm provider")
}

func TestAnthropicExtractor_DefaultModel(t *testing.T) {
	llmFake := &fakeLLM{resp: toolUseResponse(t, ExtractorToolName, scope.ScopeDelta{})}
	ext := NewAnthropicExtractor(llmFake, "")
	_, _ = ext.Extract(context.Background(), ExtractorInput{UserRequest: "x"})
	assert.Equal(t, DefaultExtractorModel, llmFake.seen.Model)
}

func TestAnthropicExtractor_ModelOverride(t *testing.T) {
	llmFake := &fakeLLM{resp: toolUseResponse(t, ExtractorToolName, scope.ScopeDelta{})}
	ext := NewAnthropicExtractor(llmFake, "claude-opus-X")
	_, _ = ext.Extract(context.Background(), ExtractorInput{UserRequest: "x"})
	assert.Equal(t, "claude-opus-X", llmFake.seen.Model)
}
