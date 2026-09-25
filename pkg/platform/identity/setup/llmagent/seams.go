package llmagent

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
)

// LLMProviderFactory builds the LLM provider for the setup agent. Tests inject a
// fake; production uses the Anthropic provider from pkg/agent/llm/anthropic.
var LLMProviderFactory = DefaultProviderFactory

// DefaultProviderFactory delegates to anthropic.NewFromEnv, which reads
// ANTHROPIC_API_KEY, and errors descriptively when the key is absent. Exported
// so tests can restore the original factory after injecting a fake:
//
//	llmagent.LLMProviderFactory = myFake
//	t.Cleanup(func() { llmagent.LLMProviderFactory = llmagent.DefaultProviderFactory })
func DefaultProviderFactory(ctx context.Context) (llm.Provider, error) {
	p, err := anthropic.NewFromEnv()
	if err != nil {
		return nil, fmt.Errorf("llmagent: LLM provider unavailable: %w", err)
	}
	return p, nil
}
