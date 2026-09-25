package promptlib

import (
	"context"
	"fmt"

	agentllm "github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/llm"
)

// jsonOnlyHint is appended to the system prompt to coax a provider with no
// structured-output mode into emitting raw JSON only — no prose, no markdown
// fences. StripFences tolerates fences anyway; the hint keeps the happy path
// clean.
const jsonOnlyHint = "\n\nIMPORTANT: respond with the JSON object ONLY — no prose, no explanations, no markdown code fences. Begin your response with `{` and end it with `}`."

// NewProvider wraps an agentllm.Provider so it satisfies the toolspec
// llm.Provider interface. Each of the four toolspec methods is one Send call
// over this package's shared prompt builders and parsers, so no backend needs
// its own copy of either.
//
// modelID is the per-call Request.Model value. Empty is rejected by Send.
func NewProvider(prov agentllm.Provider, modelID string) llm.Provider {
	return &adapter{prov: prov, modelID: modelID}
}

type adapter struct {
	prov    agentllm.Provider
	modelID string
}

func (a *adapter) SelectToolkit(ctx context.Context, req llm.SelectRequest) (*llm.SelectResponse, error) {
	sys, user := BuildSelectPrompt(req.Intent, req.ToolkitSummaries)
	raw, err := a.generateJSON(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	return ParseSelectResponse(raw)
}

func (a *adapter) GenerateSpec(ctx context.Context, req llm.GenerateRequest) (*llm.GenerateResponse, error) {
	sys, user := BuildGeneratePrompt(req)
	raw, err := a.generateJSON(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	return ParseGenerateResponse(raw)
}

func (a *adapter) GenerateTestCases(ctx context.Context, req llm.TestRequest) (*llm.TestResponse, error) {
	sys, user := BuildTestGenPrompt(req.Intent, req.ToolkitName, req.ToolkitYAML)
	raw, err := a.generateJSON(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	return ParseTestGenResponse(raw)
}

func (a *adapter) RefineSpec(ctx context.Context, req llm.RefineRequest) (*llm.GenerateResponse, error) {
	sys, user := BuildRefinePrompt(req)
	raw, err := a.generateJSON(ctx, sys, user)
	if err != nil {
		return nil, err
	}
	return ParseGenerateResponse(raw)
}

// generateJSON sends one chat-completion turn and returns the first
// non-empty text content block. The system prompt gains jsonOnlyHint
// so providers know to skip prose/fences.
func (a *adapter) generateJSON(ctx context.Context, system, user string) (string, error) {
	resp, err := a.prov.Send(ctx, agentllm.Request{
		Model: a.modelID,
		System: []agentllm.SystemBlock{{
			Text:      system + jsonOnlyHint,
			Cacheable: true,
		}},
		Messages: []agentllm.Message{{
			Role: "user",
			Content: []agentllm.ContentBlock{{
				Type: "text",
				Text: user,
			}},
		}},
		MaxTokens: 8192,
	})
	if err != nil {
		return "", fmt.Errorf("toolspec llm: %s send: %w", a.prov.Name(), err)
	}
	for _, b := range resp.Content {
		if b.Type == "text" && b.Text != "" {
			return b.Text, nil
		}
	}
	return "", fmt.Errorf("toolspec llm: %s returned no text content (stop_reason=%q)", a.prov.Name(), resp.StopReason)
}
