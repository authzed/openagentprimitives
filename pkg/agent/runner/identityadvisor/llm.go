package identityadvisor

import (
	"context"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func NewWithLLM(p llm.Provider, model string) Provider { return &neutral{p: p, model: model} }

type neutral struct {
	p     llm.Provider
	model string
}

func (n *neutral) Name() string { return n.p.Name() }
func (n *neutral) Recommend(ctx context.Context, r Request) (Recommendation, error) {
	out, err := n.p.Send(ctx, llm.Request{Model: n.model, System: []llm.SystemBlock{{Text: buildSystemPrompt()}}, Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: buildUserPrompt(r)}}}}, MaxTokens: 200})
	if err != nil {
		return Recommendation{}, err
	}
	var text strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return parseAndValidate(text.String())
}
