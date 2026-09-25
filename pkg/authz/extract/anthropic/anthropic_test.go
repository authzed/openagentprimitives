package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/extract"
	"github.com/authzed/openagentprimitives/pkg/authz/extract/anthropic"
)

// stubLLM captures Send() inputs and returns a configured response.
type stubLLM struct {
	received llm.Request
	resp     llm.Response
	err      error
}

func (s *stubLLM) Name() string                            { return "stub" }
func (s *stubLLM) SupportedFromEnv() bool                  { return true }
func (s *stubLLM) Pricing(string) (llm.ModelPricing, bool) { return llm.ModelPricing{}, false }
func (s *stubLLM) Capabilities(string) llm.CapabilitySet   { return llm.NewCapabilitySet() }
func (s *stubLLM) NativeInputMIMEs(string) llm.MIMESet     { return nil }
func (s *stubLLM) Send(_ context.Context, r llm.Request) (llm.Response, error) {
	s.received = r
	return s.resp, s.err
}

// helper: build an llm.Response that emits a single tool_use block carrying
// the given JSON-encoded extraction args.
func toolUseResponse(t *testing.T, args map[string]any) llm.Response {
	t.Helper()
	inputBytes, err := json.Marshal(args)
	require.NoError(t, err)
	return llm.Response{
		Content: []llm.ContentBlock{{
			Type: "tool_use",
			ToolUse: &llm.ToolUseBlock{
				ID:    "tool_abc",
				Name:  "extract_entities",
				Input: inputBytes,
			},
		}},
	}
}

func TestAnthropic_ExtractsEntitiesFromToolUse(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			map[string]any{
				"resource_type": "github_repo",
				"resource_id":   "foo/bar",
				"source_text":   "merge PR 17 in foo/bar",
			},
		},
	}
	stub := &stubLLM{resp: toolUseResponse(t, args)}
	p := anthropic.New(stub, "claude-haiku-4-5-20251001")

	got, err := p.Extract(context.Background(), extract.ExtractInput{
		UserMessage: "merge PR 17 in foo/bar",
		EntityTypes: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read"},
		},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "github_repo", got[0].ResourceType)
	assert.Equal(t, "foo/bar", got[0].ResourceID)
	assert.Equal(t, "claude-haiku-4-5-20251001", stub.received.Model)
	require.Len(t, stub.received.Tools, 1)
	assert.Equal(t, "extract_entities", stub.received.Tools[0].Name)
}

func TestAnthropic_ReturnsEmptyOnNoToolUse(t *testing.T) {
	// LLM responded with text only (no tool_use). Treat as zero entities.
	stub := &stubLLM{resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "text", Text: "I don't see any repos."}},
	}}
	p := anthropic.New(stub, "claude-haiku-4-5-20251001")
	got, err := p.Extract(context.Background(), extract.ExtractInput{
		UserMessage: "hi",
		EntityTypes: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read"},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAnthropic_SurfacesLLMError(t *testing.T) {
	stub := &stubLLM{err: errors.New("api down")}
	p := anthropic.New(stub, "claude-haiku-4-5-20251001")
	_, err := p.Extract(context.Background(), extract.ExtractInput{
		UserMessage: "anything",
		EntityTypes: []spiceboxv1alpha1.BoundEntityType{
			{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read"},
		},
	})
	require.Error(t, err, "expected error from underlying LLM")
}

func TestAnthropic_IncludesAlreadyBoundInPrompt(t *testing.T) {
	stub := &stubLLM{resp: toolUseResponse(t, map[string]any{"entities": []any{}})}
	p := anthropic.New(stub, "claude-haiku-4-5-20251001")
	_, _ = p.Extract(context.Background(), extract.ExtractInput{
		UserMessage:  "anything",
		EntityTypes:  []spiceboxv1alpha1.BoundEntityType{{ResourceType: "github_repo", Description: "GitHub repo", Permission: "read"}},
		AlreadyBound: []extract.SessionBinding{{ResourceType: "github_repo", ResourceID: "foo/bar"}},
	})
	// System prompt should mention foo/bar so the LLM doesn't re-extract it.
	found := false
	for _, b := range stub.received.System {
		if b.Text != "" && strings.Contains(b.Text, "foo/bar") {
			found = true
		}
	}
	assert.True(t, found, "System prompt missing already-bound foo/bar; system=%+v", stub.received.System)
}
