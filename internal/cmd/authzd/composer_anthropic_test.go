package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestAnthropicComposer_HappyPath(t *testing.T) {
	out := map[string]any{
		"approverSummary":     "If you approve, the agent will be able to read foo/bar.",
		"skippedExplanations": []string{},
		"caveatExplanations":  []string{},
	}
	llmFake := &fakeLLM{resp: toolUseResponse(t, ComposerToolName, out)}
	c := NewAnthropicComposer(llmFake, "")
	got, err := c.Compose(context.Background(), ComposerInput{Applied: scope.ScopeDelta{}})
	require.NoError(t, err)
	assert.Equal(t, "If you approve, the agent will be able to read foo/bar.", got.ApproverSummary)
}

func TestAnthropicComposer_NoRawUserTextInRequest(t *testing.T) {
	// Prompt-injection invariant: the composer LLM input must NOT contain
	// raw user-supplied strings. Verify by asserting no field name suggesting
	// user text appears in the marshaled message content and system prompt.
	//
	// Note: ComposerInput as defined has NO field for user text — that's
	// the safety property. This test asserts the property still holds.

	llmFake := &fakeLLM{resp: toolUseResponse(t, ComposerToolName, map[string]any{
		"approverSummary": "Test.",
	})}
	c := NewAnthropicComposer(llmFake, "")
	_, _ = c.Compose(context.Background(), ComposerInput{
		Applied: scope.ScopeDelta{
			Add: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}},
		},
	})

	// Inspect the message content and system prompt — the parts of the LLM
	// request that carry the actual user-visible text — and assert no field
	// name suggesting raw user text is present.
	msgBytes, err := json.Marshal(llmFake.seen.Messages)
	require.NoError(t, err)
	sysBytes, err := json.Marshal(llmFake.seen.System)
	require.NoError(t, err)
	combined := string(msgBytes) + string(sysBytes)
	for _, forbidden := range []string{"userRequest", "user_request", "rawUserText", "requesterText"} {
		assert.False(t, strings.Contains(combined, forbidden), "LLM body must not contain field %q", forbidden)
	}
}

func TestAnthropicComposer_LLMError_Fallback(t *testing.T) {
	llmFake := &fakeLLM{err: assertError("boom")}
	c := NewAnthropicComposer(llmFake, "")
	got, err := c.Compose(context.Background(), ComposerInput{})
	require.NoError(t, err, "LLM error must not propagate; fallback used")
	assert.Equal(t, fallbackApproverSummary, got.ApproverSummary)
}

func TestAnthropicComposer_NoToolCall_Fallback(t *testing.T) {
	llmFake := &fakeLLM{resp: llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: "I cannot."}}}}
	c := NewAnthropicComposer(llmFake, "")
	got, err := c.Compose(context.Background(), ComposerInput{})
	require.NoError(t, err)
	assert.Equal(t, fallbackApproverSummary, got.ApproverSummary)
}

func TestAnthropicComposer_NilLLM_Fallback(t *testing.T) {
	c := NewAnthropicComposer(nil, "")
	got, err := c.Compose(context.Background(), ComposerInput{})
	require.NoError(t, err)
	assert.Equal(t, fallbackApproverSummary, got.ApproverSummary)
}

func TestAnthropicComposer_DefaultModel(t *testing.T) {
	llmFake := &fakeLLM{resp: toolUseResponse(t, ComposerToolName, map[string]any{"approverSummary": "x"})}
	c := NewAnthropicComposer(llmFake, "")
	_, _ = c.Compose(context.Background(), ComposerInput{})
	assert.Equal(t, DefaultComposerModel, llmFake.seen.Model)
}

type assertErrorType struct{ msg string }

func (e *assertErrorType) Error() string { return e.msg }
func assertError(s string) error         { return &assertErrorType{msg: s} }
