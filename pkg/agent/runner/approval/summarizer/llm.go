package summarizer

import (
	"context"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// NewWithLLM uses an authenticated provider with an isolated request: no chat
// history, tools or primary-agent output ever enters this summarizer.
func NewWithLLM(p llm.Provider, model string) Provider {
	return &neutral{p: p, model: model, cache: NewCache(1024)}
}

type neutral struct {
	p     llm.Provider
	model string
	cache *Cache
}

func (n *neutral) Name() string { return n.p.Name() }
func (n *neutral) call(ctx context.Context, system, user string) (string, error) {
	r, e := n.p.Send(ctx, llm.Request{Model: n.model, System: []llm.SystemBlock{{Text: system}}, Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: user}}}}, MaxTokens: 200})
	if e != nil {
		return "", e
	}
	var text strings.Builder
	for _, b := range r.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	return parseAndValidate(text.String())
}
func (n *neutral) Summarize(ctx context.Context, r Request) (string, error) {
	if got, ok := n.cache.Get(r); ok {
		return got, nil
	}
	got, err := n.call(ctx, buildSystemPrompt(), buildUserPrompt(r))
	if err == nil {
		n.cache.Put(r, got)
	}
	return got, err
}
func (n *neutral) SummarizeAnnotations(ctx context.Context, r AnnotationRequest) (string, error) {
	return n.call(ctx, annotationSystemPrompt(), clampAnnotationText(r.TrustedText))
}
